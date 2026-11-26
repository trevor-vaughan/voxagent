# Generates fnmatch.json: File.fnmatch(pattern, name, DOTMATCH|PATHNAME) from the
# openvox-bundled Ruby, for every pattern x name. Run through gen.sh.
require 'json'
require 'base64'
require 'puppet/version'

def utf8(s)
  s.b.force_encoding('UTF-8')
end

patterns = [
  '*', '?', '*.old', '.*', '*x*', '[abc]', '[a-c]', '[!a]', '[^a]', '[]a]', '[!]a]',
  '[a-]', '[\\]]', '\\*', '\\?', '\\\\', 'a\\', '\\', '[', '[a', '[a-', '[a-c', '[\\',
  '{a,b}', 'a/b', '*/x', '**', '**/', '**/a', 'a/**/b', '**/**/a', '', 'é*', '[é]', '[à-ü]',
  '[a-\\]]', '[\\a-c]', '[c-a]', '[-a]', '[a-a]', '??', '?*', '*?', 'a*b', 'a*', '*a', '*.*',
  '.', '..', '/', 'a/', '/a', '*/', '[a-c]*', '\\[a]', '[[]', '[*]', '[?]', 'a?',
  'simp_generate_types*', '00-simp-tlog.*', 'sudosh2.*',
  utf8("bad\xff"), utf8("[a-\xff]"), utf8("[\xff-z]"), utf8("[\xff]"), utf8("\xff*"),
  utf8("*\xff"), utf8("[a-\xe3\x81]"), utf8("\xe3\x81"), utf8("\xe3"), utf8("a\xf0\x90"),
  "a\0b",
]

names = [
  '', '.', '..', '.hidden', 'a', 'b', 'ab', 'a.old', 'x.old.new', ']', '\\', '*', '?',
  '{a,b}', 'é', 'ée', '00-simp-tlog.sh', 'sudosh2.csh', 'simp_generate_types.rb',
  'simp_generate_types', 'a/b', 'a/b/c', 'x', 'a/x', '/a', 'a/', '-', 'c', 'ü', 'a-',
  '[', '[a]', 'a.', 'xx', 'あ', 'x/a/b', 'a/c/b', 'a/c/d/b', '.a/b',
  utf8("bad\xff"), utf8("\xff"), utf8("a\xff"), utf8("\xe3\x81"), utf8("\xe3"),
  utf8("a\xf0\x90"), "a\0b",
]

rows = []
patterns.each do |pat|
  names.each do |name|
    row = { 'pattern_b64' => Base64.strict_encode64(pat), 'name_b64' => Base64.strict_encode64(name) }
    begin
      row['match'] = File.fnmatch(pat, name, File::FNM_DOTMATCH | File::FNM_PATHNAME)
      row['error'] = nil
    rescue StandardError => e
      row['match'] = nil
      row['error'] = e.message
    end
    rows << row
  end
end

puts JSON.pretty_generate({ 'openvox' => Puppet.version, 'rows' => rows })
