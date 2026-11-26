# frozen_string_literal: true

# Generates the section corpus (spec 8.1): ini_section applied by Puppet's own
# transaction, type and provider as 6.6.1 ships them with PR #581's lib hunk
# applied (the D-105 oracle; gen.sh applies it to a temporary copy of the lib).
#
#   RUBYLIB=<patched 6.6.1 lib> ruby section.rb > section-6.6.1-pr581.json     (see gen.sh)
#
# Each case is a file's starting state, the resource's parameters and a list of
# runs; a run applies the resource in a fresh catalog, live or under noop, and
# records the events, the err logs and the file's bytes afterwards. Cases run
# under both locales M6 measured (LANG unset: US-ASCII; LANG=C.UTF-8: UTF-8) by
# setting Encoding.default_external. Byte strings are base64 (`*_b64`); the
# file's path is `<path>` in messages.
require 'json'
require 'base64'
require 'tmpdir'
require 'fileutils'
require 'puppet'

ORACLE = '8.34.0'
abort "section.rb: puppet #{Puppet.version}, want #{ORACLE}" unless Puppet.version == ORACLE

ENCODINGS = %w[US-ASCII UTF-8].freeze
CASES = []

# Puppet's transaction keeps its state file under vardir; the directory lives
# for the whole run.
STATE = Dir.mktmpdir('section-state')
at_exit { FileUtils.remove_entry(STATE) }
Puppet.initialize_settings(['--confdir', File.join(STATE, 'conf'), '--vardir', File.join(STATE, 'var')])

def b64(bytes)
  Base64.strict_encode64(bytes)
end

# add registers a case. input: a String (the file's bytes), :missing,
# :directory or :missing_parent (the file's parent does not exist). params are
# the resource's, except path. runs: one noop flag per run.
def add(name, input, params, runs = [false])
  CASES << { name: name, input: input, params: params, runs: runs }
end

def apply_once(params, path, noop)
  Puppet[:noop] = noop
  catalog = Puppet::Resource::Catalog.new
  catalog.add_resource(Puppet::Type.type(:ini_section).new(params.merge(path: path)))
  report = catalog.apply.report
  scrub = ->(s) { s.to_s.gsub(path, '<path>') }
  status = report.resource_statuses.values.first
  {
    'noop' => noop,
    'failed' => status.failed,
    'changed' => status.changed,
    'events' => status.events.map do |e|
      { 'name' => e.name.to_s, 'property' => e.property, 'status' => e.status, 'message' => scrub.call(e.message),
        'previous_value' => e.previous_value.to_s, 'desired_value' => e.desired_value.to_s }
    end,
    'err_logs' => report.logs.select { |l| l.level == :err }.map { |l| scrub.call(l.message) }
  }
end

def run_case(c, encoding)
  Encoding.default_external = Encoding.find(encoding)
  Dir.mktmpdir('section') do |dir|
    path = File.join(dir, 'f.ini')
    case c[:input]
    when String then File.binwrite(path, c[:input])
    when :directory then Dir.mkdir(path)
    when :missing_parent then path = File.join(dir, 'nodir', 'f.ini')
    when :missing then nil
    end
    runs = c[:runs].map do |noop|
      r = apply_once(c[:params], path, noop)
      r.merge('final_b64' => File.file?(path) ? b64(File.binread(path)) : nil)
    end
    input = c[:input].is_a?(String) ? { 'input_b64' => b64(c[:input]) } : { c[:input].to_s => true }
    { 'name' => c[:name], 'encoding' => encoding }.merge(input).merge('params' => c[:params].transform_keys(&:to_s), 'runs' => runs)
  end
end

BASIC = "[a]\nx = 1\ny = 2\n\n[b]\nz = 3\n\n[c]\nw = 4\n"
LIVE_AND_AGAIN = [false, false].freeze

# create: new file, existing file, the file's ending, the neighbours
add('create-new-file', :missing, { name: 's', section: 'b' }, LIVE_AND_AGAIN)
add('create-empty-file', '', { name: 's', section: 'b' }, LIVE_AND_AGAIN)
add('create-existing-file', "[a]\nx = 1\n", { name: 's', section: 'b' }, LIVE_AND_AGAIN)
add('create-existing-file-no-final-newline', "[a]\nx = 1", { name: 's', section: 'b' }, LIVE_AND_AGAIN)
add('create-after-blank-lines', "[a]\nx = 1\n\n\n", { name: 's', section: 'b' })
add('create-with-global-settings', "g = 1\n[a]\nx = 1\n", { name: 's', section: 'b' })
add('create-with-comments', "# head\n[a]\n# note\nx = 1\n; tail\n", { name: 's', section: 'b' })
add('create-crlf-file', "[a]\r\nx = 1\r\n", { name: 's', section: 'b' })
add('create-section-defaults-to-name', "[a]\nx = 1\n", { name: 'viaName' })
add('create-ensure-present-explicit', "[a]\nx = 1\n", { name: 's', section: 'b', ensure: 'present' })
add('create-name-with-regexp-chars', "[a]\nx = 1\n", { name: 's', section: 'a.b[c]*' }, LIVE_AND_AGAIN)
add('create-name-with-spaces', "[a]\nx = 1\n", { name: 's', section: ' b c ' }, LIVE_AND_AGAIN)
add('create-non-ascii-name', "[a]\nx = 1\n", { name: 's', section: "café" })
add('create-in-file-with-non-ascii', "[a]\nx = café\n", { name: 's', section: 'b' })
add('create-in-file-with-invalid-utf8', "[a]\nx = \xff\n".b, { name: 's', section: 'b' })
add('create-noop', "[a]\nx = 1\n", { name: 's', section: 'b' }, [true])
add('create-noop-new-file', :missing, { name: 's', section: 'b' }, [true])
add('create-then-noop-in-sync', "[a]\nx = 1\n", { name: 's', section: 'b' }, [false, true])

# present and in sync: Puppet's default insync? (PR #581)
add('in-sync-present', BASIC, { name: 's', section: 'b' }, LIVE_AND_AGAIN)
add('in-sync-present-noop', BASIC, { name: 's', section: 'b' }, [true])
add('in-sync-global-settings-and-section', "g = 1\n[a]\nx = 1\n", { name: 's', section: 'a' })
add('in-sync-duplicate-headers', "[a]\nx = 1\n[a]\ny = 2\n", { name: 's', section: 'a' })
add('in-sync-empty-section', "[a]\n[b]\ny = 1\n", { name: 's', section: 'a' })

# remove: settings inside, position, what is left
add('remove-with-settings-inside', BASIC, { name: 's', section: 'b', ensure: 'absent' }, LIVE_AND_AGAIN)
add('remove-first-section', BASIC, { name: 's', section: 'a', ensure: 'absent' })
add('remove-last-section', BASIC, { name: 's', section: 'c', ensure: 'absent' })
add('remove-last-section-no-final-newline', "[a]\nx = 1\n[b]\ny = 1", { name: 's', section: 'b', ensure: 'absent' })
add('remove-only-section', "[a]\nx = 1\n", { name: 's', section: 'a', ensure: 'absent' }, LIVE_AND_AGAIN)
add('remove-empty-section', "[a]\n[b]\ny = 1\n", { name: 's', section: 'a', ensure: 'absent' })
add('remove-keeps-global-settings', "g = 1\n[a]\nx = 1\n[b]\ny = 1\n", { name: 's', section: 'a', ensure: 'absent' })
add('remove-with-comments-inside', "[a]\n# note\nx = 1\n; tail\n[b]\ny = 1\n", { name: 's', section: 'a', ensure: 'absent' })
add('remove-duplicate-headers', "[a]\nx = 1\n[a]\ny = 2\n[b]\nz = 1\n", { name: 's', section: 'a', ensure: 'absent' }, LIVE_AND_AGAIN)
add('remove-crlf-file', "[a]\r\nx = 1\r\n[b]\r\ny = 1\r\n", { name: 's', section: 'a', ensure: 'absent' })
add('remove-section-with-trailing-blank-lines', "[a]\nx = 1\n\n  \n[b]\ny = 1\n", { name: 's', section: 'a', ensure: 'absent' })
add('remove-name-with-regexp-chars', "[a.b[c]*]\nx = 1\n[ab]\ny = 1\n", { name: 's', section: 'a.b[c]*', ensure: 'absent' })
add('remove-non-ascii-name', "[café]\nx = 1\n[b]\ny = 1\n", { name: 's', section: "café", ensure: 'absent' })
add('remove-noop', BASIC, { name: 's', section: 'b', ensure: 'absent' }, [true])
add('remove-then-noop-in-sync', BASIC, { name: 's', section: 'b', ensure: 'absent' }, [false, true])

# remove when absent: in sync, nothing created
add('remove-absent-section', BASIC, { name: 's', section: 'zz', ensure: 'absent' })
add('remove-absent-section-noop', BASIC, { name: 's', section: 'zz', ensure: 'absent' }, [true])
add('remove-missing-file', :missing, { name: 's', section: 'b', ensure: 'absent' })
add('remove-empty-file', '', { name: 's', section: 'b', ensure: 'absent' })

# custom prefix and suffix
add('prefix-suffix-create', "<a>\nx = 1\n", { name: 's', section: 'b', section_prefix: '<', section_suffix: '>' }, LIVE_AND_AGAIN)
add('prefix-suffix-create-multichar', "<<a>>\nx = 1\n", { name: 's', section: 'b', section_prefix: '<<', section_suffix: '>>' })
add('prefix-suffix-remove', "<a>\nx = 1\n<b>\ny = 1\n", { name: 's', section: 'a', section_prefix: '<', section_suffix: '>', ensure: 'absent' })
add('prefix-suffix-in-sync', "<a>\nx = 1\n", { name: 's', section: 'a', section_prefix: '<', section_suffix: '>' })
add('prefix-suffix-regexp-chars-create', "(a)\nx = 1\n", { name: 's', section: 'b', section_prefix: '(', section_suffix: ')' })
add('prefix-suffix-regexp-chars-remove', "(a)\nx = 1\n(b)\ny = 1\n", { name: 's', section: 'a', section_prefix: '(', section_suffix: ')', ensure: 'absent' })
add('prefix-suffix-default-headers-ignored', "[a]\nx = 1\n", { name: 's', section: 'a', section_prefix: '<', section_suffix: '>' })
add('prefix-suffix-empty', "a\nx = 1\n", { name: 's', section: 'b', section_prefix: '', section_suffix: '' })

# a section named like a setting
add('named-like-setting-create', "[a]\nb = 1\n", { name: 's', section: 'b' }, LIVE_AND_AGAIN)
add('named-like-setting-remove-section-only', "[a]\nb = 1\n[b]\nb = 2\n", { name: 's', section: 'b', ensure: 'absent' })
add('named-like-setting-absent-when-only-a-setting', "[a]\nb = 1\n", { name: 's', section: 'b', ensure: 'absent' })
add('named-like-setting-in-sync', "[b]\nb = 1\n", { name: 's', section: 'b' })
add('named-like-global-setting', "b = 1\n[a]\nx = 1\n", { name: 's', section: 'b' })

# failures: the write, the read
add('create-missing-parent', :missing_parent, { name: 's', section: 'b' })
add('create-directory-at-path', :directory, { name: 's', section: 'b' })
add('remove-directory-at-path', :directory, { name: 's', section: 'b', ensure: 'absent' })
add('create-missing-parent-noop', :missing_parent, { name: 's', section: 'b' }, [true])

puts JSON.pretty_generate('openvox' => Puppet.version, 'ruby' => RUBY_VERSION, 'cases' => CASES.flat_map { |c| ENCODINGS.map { |enc| run_case(c, enc) } })
