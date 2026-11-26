# frozen_string_literal: true

# Generates the subsetting corpus (spec 8.1): Puppet::Util::SettingValue as the
# ini_subsetting provider drives it (pss: exists?/create/destroy/value=).
#
#   RUBYLIB=<release>/lib ruby subsetting.rb > subsetting-<release>.json     (see gen.sh)
#
# Each case is a SettingValue's constructor arguments and a list of calls on
# it. Every call records its result or the exception it raised; a constructor
# that raises records `ctor_error` and no calls. The mutators' return values
# (an Array the providers ignore) are not recorded: a case reads the outcome
# with get_value. A call is [op, *args]; use_exact_match is a boolean here and
# the symbols :true/:false in the call, and insert_type a String and the
# Symbol. get_subsetting_value's `true` (exact match found) is recorded as JSON
# true.
require 'json'
require 'puppet'
require 'puppet/util/setting_value'

ORACLE = '8.34.0'
abort "subsetting.rb: puppet #{Puppet.version}, want #{ORACLE}" unless Puppet.version == ORACLE

SETTING_VALUE = Puppet::Util::SettingValue
CASES = []

# add registers a case. ctor: value (String or nil), separator, quote_char,
# key_val_separator, as the provider passes them.
def add(name, value, ops, sep: ' ', quote: '', kvs: '')
  CASES << { name: name, ctor: { 'value' => value, 'sep' => sep, 'quote' => quote, 'kvs' => kvs }, ops: ops }
end

def sym(flag)
  flag ? :true : :false
end

def call(sv, op, args)
  case op
  when 'get_value' then sv.get_value
  when 'get_subsetting_value' then sv.get_subsetting_value(args[0], sym(args[1]))
  when 'add_subsetting'
    sub, val, exact, type, insert_value = args
    sv.add_subsetting(sub, val, sym(exact), type.to_sym, insert_value) && nil
  when 'remove_subsetting' then sv.remove_subsetting(args[0], sym(args[1])) && nil
  else raise "subsetting.rb: unknown op #{op}"
  end
end

def run_case(c)
  k = c[:ctor]
  entry = { 'name' => c[:name], 'ctor' => k }
  begin
    sv = SETTING_VALUE.new(k['value'], k['sep'], k['quote'], k['kvs'])
  rescue StandardError => e
    return entry.merge('ctor_error' => { 'class' => e.class.name, 'message' => e.message }, 'ops' => [])
  end
  ops = c[:ops].map do |op, *args|
    o = { 'op' => op, 'args' => args }
    begin
      o['result'] = call(sv, op, args)
    rescue StandardError => e
      o['error'] = { 'class' => e.class.name, 'message' => e.message }
    end
    o
  end
  entry.merge('ops' => ops)
end

# Call builders: read, get, put (add_subsetting) and del (remove_subsetting);
# every case ends a mutation with read, the outcome the providers write.
def read = ['get_value']
def get(sub, exact = false) = ['get_subsetting_value', sub, exact]
def put(sub, val, type = 'end', ins = nil, exact: false) = ['add_subsetting', sub, val, exact, type, ins]
def del(sub, exact = false) = ['remove_subsetting', sub, exact]

JAVA = '-Xms256m -Xmx512m -XX:+UseG1GC'

# construction and get_value: quoting, separators, empty and nil values
add('nil-value', nil, [read, get('a'), get('a', true)])
add('empty-value', '', [read, get('a'), put('a', '1'), read])
add('plain-value', JAVA, [read])
add('double-quoted', "\"#{JAVA}\"", [read])
add('single-quoted', "'#{JAVA}'", [read])
add('empty-double-quotes', '""', [read, put('a', '1'), read])
add('empty-single-quotes', "''", [read, put('a', '1'), read])
add('lone-double-quote', '"', [read])
add('lone-single-quote', "'", [read])
add('mismatched-quotes', "\"a b'", [read])
add('leading-quote-only', '"a b', [read])
add('trailing-quote-only', 'a b"', [read])
add('quote-inside', 'a "b c" d', [read])
add('default-quote-double-unquoted', 'a b', [read, put('c', nil), read], quote: '"')
add('default-quote-single-unquoted', 'a b', [read, put('c', nil), read], quote: "'")
add('default-quote-double-empty-value', '', [read, put('c', nil), read], quote: '"')
add('default-quote-nil-value', nil, [read, put('c', nil), read], quote: '"')
add('default-quote-nil-value-single', nil, [read, put('c', nil), read], quote: "'")
add('default-quote-overridden-by-value', "'a b'", [read, put('c', nil), read], quote: '"')
add('default-quote-same-as-value', '"a b"', [read, put('c', nil), read], quote: '"')
add('default-quote-lone-quote', '"', [read], quote: "'")
add('whitespace-runs', 'a   b  c', [read])
add('leading-trailing-space', '  a b  ', [read])
add('tab-in-value', "a\tb c", [read])
add('tab-separator', "a\tb\tc", [read, put('d', nil), read], sep: "\t")
add('comma-separator', 'a,b,c', [read, put('d', nil), read], sep: ',')
add('comma-separator-spaced', 'a , b ,c', [read, put('d', nil), read], sep: ',')
add('comma-space-separator', 'a, b,c d', [read, put('e', nil), read], sep: ', ')
add('semicolon-separator', 'a;b;c', [read, put('d', nil), read], sep: ';')
add('colon-separator-quoted', '"/a:/b:/c"', [read, put('/d', nil), read], sep: ':')
add('dot-separator-is-not-a-separator', 'a.b.c', [read, get('a'), put('d', nil), read], sep: '.')
add('empty-separator', 'a b', [read], sep: '')
# Separators supportedSeparator accepts that no other case covers (Task 8).
add('space-letter-separator', 'xa yb zc', [read, get('x'), put('d', nil), read, del('y'), read], sep: ' a')
add('backslash-separator', 'a\\b\\c', [read, put('d', nil), read, del('b'), read], sep: '\\')
add('multibyte-separator', 'aéb é c', [read, put('d', nil), read], sep: 'é')
add('pipe-separator', 'a|b|c', [read, put('d', nil), read, del('b'), read], sep: '|')
add('separator-not-in-value', 'a b c', [read], sep: ',')
add('only-separators', ' , ,', [read, put('a', nil), read], sep: ',')
add('multibyte-items', 'é=1 ü=2', [read, get('é', false), get('ü', false), put('é', '3'), read], kvs: '=')
add('newline-in-value', "a\nb c", [read, put('d', nil), read])

# get_subsetting_value: exact versus prefix match, values, missing
add('get-prefix-match', JAVA, [get('-Xmx'), get('-Xm'), get('-Xms'), get('-X'), get('-XX:+UseG1GC'), get('-Xmx512m'), get('-Xmy'), get('')])
add('get-exact-match', JAVA, [get('-Xmx', true), get('-Xmx512m', true), get('-XX:+UseG1GC', true), get('-Xm', true), get('', true), get('-xmx512m', true)])
add('get-key-val-separator', 'a=1 b=2 c=', [get('a'), get('b'), get('c'), get('d'), get('a='), get('a', true), get('a=1', true)], kvs: '=')
add('get-key-val-separator-colon-space', 'a: 1 b: 2', [get('a'), get('b')], kvs: ': ')
add('get-key-val-separator-no-match-without-it', 'a1 b2', [get('a'), get('b')], kvs: '=')
add('get-first-of-duplicates', 'a=1 a=2', [get('a')], kvs: '=')
add('get-from-nil-value', nil, [get('a'), get('a', true)])
add('get-from-quoted', '"a=1 b=2"', [get('b')], kvs: '=')
add('get-case-sensitive', 'Abc=1', [get('abc'), get('Abc')], kvs: '=')
add('get-empty-subsetting-name', 'a=1 =2', [get(''), get('', true)], kvs: '=')
add('get-name-longer-than-item', 'a', [get('ab'), get('ab', true)])

# add_subsetting: no match, value forms, update of an existing item
add('add-end-default', 'a b', [put('c', nil), read, put('d', 'v'), read])
add('add-with-value-and-separator', 'a=1', [put('b', '2'), read, put('c', ''), read, put('d', nil), read], kvs: '=')
add('add-value-without-separator', 'a', [put('b', '2'), read, put('c', 'x y'), read])
add('add-to-empty', '', [put('a', '1'), read], kvs: '=')
add('add-to-nil', nil, [put('a', '1'), read], kvs: '=')
add('add-to-nil-quoted', nil, [put('a', '1'), read], kvs: '=', quote: '"')
add('add-existing-updates', 'a=1 b=2 c=3', [put('b', '9'), read, put('b', nil), read], kvs: '=')
add('add-existing-updates-all-prefix-matches', '-Xmx1 -Xmy2 -Xmz3', [put('-Xm', nil), read])
add('add-existing-prefix-updates-first-and-duplicates', 'a=1 a=2 b=3', [put('a', '9'), read], kvs: '=')
add('add-prefix-name-is-not-a-different-item', '-Xmx512m', [put('-Xm', nil), read, put('-Xmx', nil, 'start'), read])
add('add-exact-does-not-match-prefix', '-Xmx512m', [put('-Xmx', nil, 'end', nil, exact: true), read])
add('add-exact-updates-exact-item', '-Xmx512m a', [put('-Xmx512m', nil, 'end', nil, exact: true), read, put('a', 'q', 'end', nil, exact: true), read])
add('add-exact-with-key-val-separator', 'a=1 a', [put('a', '2', 'end', nil, exact: true), read], kvs: '=')
add('add-existing-ignores-insert-type', 'a b c', [put('b', '1', 'start'), read, put('c', '1', 'index', '0'), read])

# every insert_type; insert_value present, absent and not found
%w[start end].each do |t|
  add("insert-#{t}", 'a b c', [put('x', nil, t), read])
  add("insert-#{t}-with-insert-value", 'a b c', [put('x', nil, t, 'b'), read])
  add("insert-#{t}-empty-list", '', [put('x', nil, t), read])
  add("insert-#{t}-nil-list", nil, [put('x', nil, t), read])
end
%w[before after].each do |t|
  add("insert-#{t}-first", 'a b c', [put('x', nil, t, 'a'), read])
  add("insert-#{t}-middle", 'a b c', [put('x', nil, t, 'b'), read])
  add("insert-#{t}-last", 'a b c', [put('x', nil, t, 'c'), read])
  add("insert-#{t}-not-found", 'a b c', [put('x', nil, t, 'zz'), read])
  add("insert-#{t}-insert-value-nil", 'a b c', [put('x', nil, t, nil), read])
  add("insert-#{t}-insert-value-nil-empty-list", '', [put('x', nil, t, nil), read])
  add("insert-#{t}-insert-value-nil-exact", 'a b c', [put('x', nil, t, nil, exact: true), read])
  add("insert-#{t}-insert-value-empty", 'a b c', [put('x', nil, t, ''), read])
  add("insert-#{t}-prefix-matches-first", 'k=1 k=2 z', [put('x', nil, t, 'k'), read], kvs: '=')
  add("insert-#{t}-with-key-val-separator", 'a=1 b=2 c=3', [put('x', '9', t, 'b'), read], kvs: '=')
  add("insert-#{t}-key-val-separator-needs-it", 'a=1 b=2', [put('x', '9', t, 'a=1'), read], kvs: '=')
  add("insert-#{t}-exact", 'a b c', [put('x', nil, t, 'b', exact: true), read])
  add("insert-#{t}-exact-needs-whole-item", 'a=1 b=2', [put('x', nil, t, 'b', exact: true), read, put('y', nil, t, 'b=2', exact: true), read], kvs: '=')
  add("insert-#{t}-exact-not-found", 'a b c', [put('x', nil, t, 'zz', exact: true), read])
  add("insert-#{t}-empty-list", '', [put('x', nil, t, 'a'), read])
  add("insert-#{t}-nil-list", nil, [put('x', nil, t, 'a'), read])
  add("insert-#{t}-quoted", '"a b c"', [put('x', nil, t, 'b'), read])
  add("insert-#{t}-separator-comma", 'a,b,c', [put('x', nil, t, 'b'), read], sep: ',')
end
add('insert-after-uses-first-match', 'a b a', [put('x', nil, 'after', 'a'), read])
add('insert-before-uses-first-match', 'a b a', [put('x', nil, 'before', 'a'), read])
add('insert-before-the-subsetting-itself-missing', 'a b', [put('b2', nil, 'before', 'b'), read])
['0', '1', '2', '3', '4', '99', '-1', '-2', '-3', '-4', '-5',
 nil, '', 'x', '2x', ' 2', '+2', '1_0', '0d2', '1.9', '0x2', '-0'].each do |v|
  add("insert-index-#{v.inspect}", 'a b c', [put('x', nil, 'index', v), read])
end
add('insert-index-empty-list', '', [put('x', nil, 'index', '3'), read])
add('insert-index-nil-list', nil, [put('x', nil, 'index', '3'), read])
add('insert-index-negative-empty-list', '', [put('x', nil, 'index', '-1'), read, put('y', nil, 'index', '-3'), read])
add('insert-index-exact', 'a b c', [put('x', nil, 'index', '1', exact: true), read])
add('insert-index-quoted', "'a b c'", [put('x', 'v', 'index', '1'), read], kvs: '=')
add('insert-index-huge', 'a b c', [put('x', nil, 'index', '99999999999999999999999'), read])
add('insert-unknown-type-appends', 'a b c', [put('x', nil, 'bogus', 'a'), read])

# remove_subsetting: prefix, exact, duplicates, nothing left
add('remove-one', JAVA, [del('-Xmx'), read])
add('remove-prefix-removes-all-matches', '-Xmx1 -Xmy2 -Xmz3', [del('-Xm'), read])
add('remove-prefix-matches-name-and-value', '-Xmx512m -Xms1', [del('-Xmx512m'), read])
add('remove-exact', JAVA, [del('-Xmx512m', true), read, del('-Xms256m', true), read])
add('remove-exact-does-not-match-prefix', JAVA, [del('-Xmx', true), read])
add('remove-absent', JAVA, [del('-Q'), read, del('-Q', true), read])
add('remove-duplicates', 'a=1 b=2 a=3', [del('a'), read], kvs: '=')
add('remove-with-key-val-separator', 'a=1 ab=2 b=3', [del('a'), read], kvs: '=')
add('remove-key-val-separator-needs-it', 'a1 b2', [del('a'), read], kvs: '=')
add('remove-last-leaves-empty', 'a', [del('a'), read])
add('remove-last-leaves-empty-quoted', '"a"', [del('a'), read])
add('remove-last-leaves-empty-default-quote', 'a', [del('a'), read], quote: '"')
add('remove-from-empty', '', [del('a'), read])
add('remove-from-nil', nil, [del('a'), read])
add('remove-empty-name-removes-everything', 'a b c', [del(''), read])
add('remove-empty-name-exact-removes-nothing', 'a b c', [del('', true), read])
add('remove-then-add', 'a=1 b=2', [del('a'), read, put('a', '5'), read], kvs: '=')
add('remove-quoted-comma', '"a,b,c"', [del('b'), read], sep: ',')

# sequences as the provider drives them
add('sequence-add-update-remove', 'a=1', [put('b', '2'), read, put('b', '3'), read, del('a'), read, get('b'), get('a')], kvs: '=')
add('sequence-get-after-add', nil, [put('-Xmx', '1g', 'end'), get('-Xmx'), get('-Xmx', true), get('-Xmx1g', true), read], kvs: '')

puts JSON.pretty_generate('openvox' => Puppet.version, 'ruby' => RUBY_VERSION, 'cases' => CASES.map { |c| run_case(c) })
