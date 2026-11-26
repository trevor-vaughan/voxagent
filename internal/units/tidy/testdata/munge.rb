# Generates munge.json: what openvox's type/tidy.rb makes of each parameter
# value, munged or refused, from Puppet::Type.type(:tidy).new. Run through
# munge.sh. Values are tagged with their Ruby class so the Go test can build
# the catalog value Ruby saw; Integers are decimal strings, since a Bignum does
# not survive a JSON float.
require 'json'
require 'digest'
require 'puppet'

def tag(v)
  case v
  when nil then { 'kind' => 'nil' }
  when true, false then { 'kind' => 'Boolean', 'value' => v }
  when Integer then { 'kind' => 'Integer', 'value' => v.to_s }
  when Float then { 'kind' => 'Float', 'value' => v }
  when String then { 'kind' => 'String', 'value' => v }
  when Symbol then { 'kind' => 'Symbol', 'value' => v.to_s }
  when Array then { 'kind' => 'Array', 'value' => v.map { |e| tag(e) } }
  else raise "untaggable #{v.inspect}"
  end
end

ABSENT = Object.new

# Each case is [param, value, other parameters]; ABSENT leaves param unset,
# so the row records Ruby's default.
cases = []
add = ->(param, values, others = {}) { values.each { |v| cases << [param, v, others] } }

add.call(:path, ['/tmp//t/../t/', '/', '/tmp/t/', '/tmp/t', '/a/./b', '/..', '/a/../..', '/tmp/~x',
                 "/a\0b", 't/rel', '~/x', '//tmp/x', '///x'])
add.call(:recurse, [true, 'true', false, 'false', 'inf', 'Inf', 'TRUE', 2, '2', '010', '08', 'yes', 0, -1,
                    '0', '00', '-1', 1, -5, '', 1.5, ['1'], '99999999999999999999', "1\n", ABSENT])
add.call(:max_files, [5, '5', '-1', 'x', 0, '0', '00', -1, 1.5, true, ['5'], "5\n", ABSENT], { recurse: true })
['*.x', '*'].each do |m|
  [0, false, ABSENT, '0', '00'].each { |rec| cases << [:matches, m, rec.equal?(ABSENT) ? {} : { recurse: rec }] }
end
add.call(:matches, ['*', ['a', 'b'], [['a'], 'b'], [], [1], "a\nb"], { recurse: true })
[-1, 'inf', 2, '010', 'true'].each { |rec| cases << [:matches, '*', { recurse: rec }] }
add.call(:backup, ['puppet', true, false, ['x']], { recurse: true })
unit_inputs = ['', '0', '1', '10', '1w', '1W', '3hours', '99999999999999999999d', '007', '1 d', ' 1d', true, false,
               '1x', '1_', '01', 'abc', 10, 1.5, '08d', '010d', '08k', '010k', '1.5', ['1d'], "1d\r", '1é',
               "1d\n", ABSENT]
add.call(:age, unit_inputs + ['1d', '2weeks', '1D', '1s', '1m', '1M', '1h', '2minutes', '99999999999999999999w'])
add.call(:size, unit_inputs + ['1k', '1b', '1B', '1m', '1g', '1t', '1T', '2kilobytes', '9999999999t', '99999999999999999t'])
add.call(:type, ['atime', 'mtime', 'ctime', 'bogus', 'ATIME', true, 1, ['atime'], '', ABSENT], { age: '1s' })
add.call(:rmdirs, [true, false, 'true', 'false', 'yes', 'no', 'on', '1', 'TRUE', 1, ['true'], '', ABSENT])

rows = cases.map do |param, value, others|
  args = param == :path ? {} : { path: '/x' }
  args.merge!(others)
  args[param] = value unless value.equal?(ABSENT)
  row = { 'param' => param.to_s, 'params' => args.to_h { |k, v| [k.to_s, tag(v)] } }
  begin
    munged = Puppet::Type.type(:tidy).new(args)[param]
    row['munged'] = tag(munged)
    row['inspect'] = munged.inspect
  rescue StandardError => e
    # A NameError's text carries object addresses; they are zeroed so a rerun
    # reproduces the file.
    row['error'] = e.message.gsub(/0x\h{16}/, '0x0000000000000000')
  end
  row
end

puts JSON.pretty_generate({
  'openvox' => Puppet.version,
  'script_sha256' => Digest::SHA256.file(__FILE__).hexdigest,
  'rows' => rows,
})
