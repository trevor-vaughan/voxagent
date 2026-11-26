# frozen_string_literal: true

# Generates the munge corpus: every parameter of ini_setting, ini_subsetting and
# ini_section, valid and invalid values, as Puppet converts a catalog resource.
#
#   RUBYLIB=<release>/lib ruby munge.rb > munge-<release>.json     (see gen.sh)
#
# Each case gives a resource's parameters (plus the names the catalog marks
# Sensitive) and records either the resulting value of every attribute or the
# error text Puppet prints. The resource goes through Puppet::Resource#to_ral,
# as the agent's catalog conversion does, so Sensitive is a catalog mark and
# the value reaches the type plain (a Ruby Sensitive object would be unwrapped
# by `value` only). Values are tagged {"t": <class>, "v": ...}.
Encoding.default_external = Encoding::UTF_8
Encoding.default_internal = Encoding::UTF_8

require 'json'
require 'puppet'

ORACLE = '8.34.0'
abort "munge.rb: puppet #{Puppet.version}, want #{ORACLE}" unless Puppet.version == ORACLE

# tag encodes a Ruby value as the corpus does.
def tag(value)
  case value
  when nil then { 't' => 'Nil' }
  when Symbol then { 't' => 'Symbol', 'v' => value.to_s }
  when true, false then { 't' => 'Boolean', 'v' => value }
  when Integer then { 't' => 'Integer', 'v' => value }
  when Float then { 't' => 'Float', 'v' => value }
  when String then { 't' => 'String', 'v' => value }
  when Array then { 't' => 'Array', 'v' => value.map { |e| tag(e) } }
  else raise "munge.rb: cannot tag #{value.class}"
  end
end

# RESOURCE gives each type's required parameters and every attribute (the
# properties first, then the parameters, in the type's definition order).
RESOURCE = {
  'ini_setting' => {
    required: { path: '/x', setting: 's' },
    attrs: %i[ensure value name section setting force_new_section_creation path show_diff key_val_separator
              section_prefix section_suffix indent_char indent_width refreshonly]
  },
  'ini_subsetting' => {
    required: { path: '/x', setting: 's', subsetting: 'ss' },
    attrs: %i[ensure value name section setting subsetting subsetting_separator subsetting_key_val_separator
              path show_diff key_val_separator quote_char use_exact_match insert_type insert_value delete_if_empty]
  },
  'ini_section' => {
    required: { path: '/x' },
    attrs: %i[ensure name section path section_prefix section_suffix]
  }
}.freeze

def run_case(type, given, sensitive)
  spec = RESOURCE.fetch(type)
  params = spec[:required].merge(given)
  resource = Puppet::Resource.new(type.to_sym, 'n', parameters: params)
  resource.sensitive_parameters = sensitive
  entry = { 'type' => type, 'given' => given.to_h { |k, v| [k.to_s, tag(v)] }, 'sensitive' => sensitive.map(&:to_s) }
  begin
    ral = resource.to_ral
    entry['result'] = { 'attrs' => spec[:attrs].to_h { |a| [a.to_s, tag(ral[a])] } }
  rescue StandardError => e
    entry['result'] = { 'error' => e.message, 'class' => e.class.name }
  end
  entry
end

CASES = []

def add(type, given, sensitive = [])
  CASES << run_case(type, given, sensitive)
end

# each adds one case per value of a parameter.
def each(type, param, values, sensitive = [])
  values.each { |v| add(type, { param => v }, sensitive) }
end

BOOLS = [true, false, 'true', 'false', 'yes', 'no', 'True', 'NO', 'bogus', '', 1, 1.5, [true], ['true', 'x']].freeze
SHOW_DIFF = [true, false, 'true', 'false', 'yes', 'no', 'md5', 'MD5', 'bogus', '', 1, [true], %w[md5 x]].freeze
STRINGS = ['x', '', ' x '].freeze
PATHS = ['/x', 'x', 'C:/x', '//x', '', '/', 'x/y', "x\n/y", "/x\n", 1, 1.5, true, ['/x'], ['x']].freeze

# ini_setting
add('ini_setting', {})
each('ini_setting', :ensure, %w[present absent bogus ABSENT] + ['', 1, true, %w[absent present], %w[absent bogus], %w[bogus absent], []])
each('ini_setting', :section, STRINGS)
each('ini_setting', :setting, ['a', ' a', 'a ', "a\t", "\ta", "a\n", ' a ', 'a b', '', ' ', "a\n b", "a \nb", "a\nb", "a\0", "\0a", "a\vb", "a\f", 1, 1.5, true, ['a']])
each('ini_setting', :force_new_section_creation, BOOLS)
each('ini_setting', :path, PATHS)
each('ini_setting', :show_diff, SHOW_DIFF)
each('ini_setting', :key_val_separator, [':', '', ' '])
each('ini_setting', :value, [true, false, 1, 1.5, -2, ' x ', "\tx\n", "\0x\0", '', ' ', 'a b', ['a', ' b '], [], [1, true, 2.5, ' c '], [nil], ['a', nil], [['a']], [[]], [1.5, 0.1]])
each('ini_setting', :section_prefix, ['<', ''])
each('ini_setting', :section_suffix, ['>', ''])
each('ini_setting', :indent_char, ["\t", '', 'ab'])
each('ini_setting', :indent_width, [2, '2', 'x', ' 3', '-1', -1, 0, 1.5, '1_0', '07', '0x1', '1e2', '+3', true, ['2']])
each('ini_setting', :refreshonly, BOOLS)
# Sensitive: the value is plain in the catalog and marked by name.
each('ini_setting', :value, ['s', ' s '], [:value])
each('ini_setting', :value, [['s', ' t ']], [:value])
each('ini_setting', :value, [[nil]], [:value])
each('ini_setting', :path, ['rel'], [:path])
each('ini_setting', :show_diff, ['bogus'], [:show_diff])
each('ini_setting', :setting, [' s'], [:setting])
each('ini_setting', :indent_width, ['x'], [:indent_width])
# Which failure comes first when several parameters are invalid (the order of
# allattrs: ensure, value, then the parameters as defined).
add('ini_setting', { path: 'rel', show_diff: 'bogus' })
add('ini_setting', { show_diff: 'bogus', setting: ' s' })
add('ini_setting', { ensure: 'bogus', value: nil, path: 'rel' })
add('ini_setting', { value: [nil], ensure: 'bogus' })
add('ini_setting', { setting: ' s', path: 'rel' })
add('ini_setting', { section: 'a', setting: ' s', value: ' v ', key_val_separator: ':', indent_char: "\t", indent_width: '4', refreshonly: 'yes', force_new_section_creation: 'no', show_diff: 'md5' })

# ini_subsetting
add('ini_subsetting', {})
each('ini_subsetting', :ensure, %w[present absent bogus] + ['', 1, true, %w[absent present], %w[bogus absent], []])
each('ini_subsetting', :section, STRINGS)
each('ini_subsetting', :setting, ['a', ' a', 1])
each('ini_subsetting', :subsetting, ['a', ' a', '', 1])
each('ini_subsetting', :subsetting_separator, [',', ''])
each('ini_subsetting', :subsetting_key_val_separator, ['=', ''])
each('ini_subsetting', :path, PATHS)
each('ini_subsetting', :show_diff, SHOW_DIFF)
each('ini_subsetting', :key_val_separator, [':', ''])
each('ini_subsetting', :quote_char, ['', '"', "'", 'x', '""', "'\"", "\n", '"x', 1, true, ['"']])
each('ini_subsetting', :use_exact_match, [true, false, 'true', 'false', 'yes', 'no', 'True', 'bogus', ''])
each('ini_subsetting', :value, ['v', ' v ', '', 1, 1.5, true, false, ['a'], %w[a b], [], [nil]])
each('ini_subsetting', :insert_type, %w[start end before after index bogus Start] + ['', 1, ['end'], %w[bogus end]])
each('ini_subsetting', :insert_value, ['a', '', 1, 1.5, true, ['a']])
each('ini_subsetting', :delete_if_empty, [true, false, 'true', 'false', 'yes', 'no', 'bogus', ''])
each('ini_subsetting', :value, ['s'], [:value])
each('ini_subsetting', :path, ['rel'], [:path])
each('ini_subsetting', :quote_char, ['x'], [:quote_char])
add('ini_subsetting', { quote_char: 'x', path: 'rel' })
add('ini_subsetting', { insert_type: 'bogus', quote_char: 'x' })
add('ini_subsetting', { show_diff: 'bogus', use_exact_match: 'bogus', delete_if_empty: 'bogus' })
add('ini_subsetting', { section: 'a', value: 'v', quote_char: '"', use_exact_match: 'true', insert_type: 'after', insert_value: 'q', delete_if_empty: 'true', subsetting_separator: ',', subsetting_key_val_separator: '=' })

# ini_section: the type exists from 6.6.0, so a 6.5.0 run records only an
# error for an unknown type.
if Puppet::Type.type(:ini_section)
  add('ini_section', {})
  each('ini_section', :ensure, %w[present absent bogus] + ['', 1, true, %w[absent present], %w[bogus absent], []])
  each('ini_section', :section, ['s', '  ', '', ' s ', "\0", 1, true, ['s']])
  each('ini_section', :path, PATHS)
  each('ini_section', :section_prefix, ['<', ''])
  each('ini_section', :section_suffix, ['>', ''])
  # The section defaults to the name, and the default is validated too.
  each('ini_section', :name, ['x y', '  ', ' x '])
  add('ini_section', { name: 'x y', section: 'z' })
  add('ini_section', { section: ' ', path: 'rel' })
  add('ini_section', { ensure: 'bogus', section: ' ' })
end

puts JSON.pretty_generate('openvox' => Puppet.version, 'ruby' => RUBY_VERSION, 'cases' => CASES)
