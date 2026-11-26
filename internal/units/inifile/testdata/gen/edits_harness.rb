# frozen_string_literal: true

# Harness of the edits and removal corpora: Puppet::Util::IniFile driven as the
# ini_setting, ini_subsetting and ini_section providers drive it (spec 8.1).
# edits.rb and removal.rb require it, add cases and call emit.
#
# Each case is a file's starting state, the arguments of IniFile.new and a list
# of calls on it. Every call records its result or the exception it raised, and
# the case records the file's bytes after the last call. The providers' call
# sequences are written out by the helpers below (ps:35-90, pss:12-50), so the
# Go port is driven by IniFile calls alone:
#
#   exists?   section_names / section? / get_value        (ps:35-54)
#   create    exists? + set_value + save + reopen         (ps:56-64)
#   value=    exists? + get_value + set_value + save      (ps:83-90)
#   destroy   exists? + remove_setting + save + reopen    (ps:66-70)
#
# `reopen` is the providers' `@ini_file = nil`: the next call builds a new
# IniFile from the file as it is. Cases run under both locales M6 measured
# (LANG unset: US-ASCII; LANG=C.UTF-8: UTF-8) by setting
# Encoding.default_external, which is what the locale sets. Byte strings are
# base64 (`*_b64`); the file's path is `<path>` in messages.
require 'json'
require 'base64'
require 'tmpdir'
require 'fileutils'
require 'puppet'
require 'puppet/util/ini_file'

ORACLE = '8.34.0'
abort "edits.rb: puppet #{Puppet.version}, want #{ORACLE}" unless Puppet.version == ORACLE

ENCODINGS = %w[US-ASCII UTF-8].freeze
INI_FILE = Puppet::Util::IniFile

# 6.6.x's remove_setting removes an emptied section unless told otherwise; the
# providers pass remove_empty_section: false (they have no such argument on
# 6.5.0, where the call below is the plain one). A third argument in a
# remove_setting call (removal corpus, 6.6.x only) is that keyword's value.
REMOVE_SECTION_KW = INI_FILE.instance_method(:remove_setting).parameters.any? { |_, n| n == :remove_empty_section }

DEFAULT_CTOR = { 'kvs' => ' = ', 'prefix' => '[', 'suffix' => ']', 'indent_char' => ' ', 'indent_width' => nil }.freeze

CASES = []

def b64(bytes)
  Base64.strict_encode64(bytes)
end

# A call is [op, *args]; the helpers below return lists of calls.
def exists_setting(section, setting)
  [['section?', section], ['get_value', section, setting]]
end

def setting_create(section, setting, value, sep: ' = ')
  exists_setting(section, setting) + [['set_value', section, setting, sep, value], ['save'], ['reopen']]
end

def setting_update(section, setting, value, sep: ' = ')
  exists_setting(section, setting) + [['get_value', section, setting], ['set_value', section, setting, sep, value], ['save']]
end

def setting_destroy(section, setting)
  exists_setting(section, setting) + [['remove_setting', section, setting], ['save'], ['reopen']]
end

def section_only_create(section)
  [['section_names'], ['set_value', section], ['save'], ['reopen']]
end

def section_only_destroy(section)
  [['section_names'], ['remove_setting', section, nil], ['save'], ['reopen']]
end

# add registers a case. input: a String (the file's bytes), :missing,
# :directory or :missing_parent (the file's parent does not exist).
def add(name, input, ops, ctor = {})
  CASES << { name: name, input: input, ctor: DEFAULT_CTOR.merge(ctor), ops: ops }
end

def call(ini, op, args)
  case op
  when 'section_names' then ini.section_names
  when 'section?' then ini.section?(*args)
  when 'get_value' then ini.get_value(*args)
  # The methods that edit return whatever their last expression is; the
  # providers ignore it, so the corpus does not record it.
  when 'set_value' then ini.set_value(*args) && nil
  when 'remove_setting'
    section, setting, remove_empty = args
    remove_empty = false if remove_empty.nil?
    (REMOVE_SECTION_KW ? ini.remove_setting(section, setting, remove_empty_section: remove_empty) : ini.remove_setting(section, setting)) && nil
  when 'remove_section' then ini.remove_section(*args) && nil
  when 'save' then ini.save && nil
  else raise "edits.rb: unknown op #{op}"
  end
end

def run_case(c, encoding)
  Encoding.default_external = Encoding.find(encoding)
  Dir.mktmpdir('edits') do |dir|
    path = File.join(dir, 'f.ini')
    case c[:input]
    when String then File.binwrite(path, c[:input])
    when :directory then Dir.mkdir(path)
    when :missing_parent then path = File.join(dir, 'nodir', 'f.ini')
    when :missing then nil
    end
    k = c[:ctor]
    ini = nil
    results = c[:ops].map do |op, *args|
      entry = { 'op' => op, 'args' => args }
      begin
        if op == 'reopen'
          ini = nil
        else
          ini ||= INI_FILE.new(path, k['kvs'], k['prefix'], k['suffix'], k['indent_char'], k['indent_width'])
          # section_names is the IniFile's own Array, which later calls change.
          result = call(ini, op, args)
          entry['result'] = result.is_a?(Array) ? result.dup : result
        end
      rescue StandardError => e
        entry['error'] = { 'class' => e.class.name, 'message' => e.message.gsub(path, '<path>') }
      end
      entry
    end
    final = File.file?(path) ? b64(File.binread(path)) : nil
    input = c[:input].is_a?(String) ? { 'input_b64' => b64(c[:input]) } : { c[:input].to_s => true }
    { 'name' => c[:name], 'encoding' => encoding }.merge(input).merge('ctor' => k, 'ops' => results, 'final_b64' => final)
  end
end

def emit
  cases = CASES.flat_map { |c| ENCODINGS.map { |enc| run_case(c, enc) } }
  puts JSON.pretty_generate('openvox' => Puppet.version, 'ruby' => RUBY_VERSION, 'cases' => cases)
end
