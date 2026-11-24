concat_file { '/etc/c04t.conf': ensure_newline => true }
concat_file { '/etc/c04f.conf': ensure_newline => false }
concat_file { '/etc/c04d.conf': }
['/etc/c04t.conf', '/etc/c04f.conf', '/etc/c04d.conf'].each |$i, $t| {
  concat_fragment { "nonl-${i}":   target => $t, order => 1, content => 'no newline' }
  concat_fragment { "nl-${i}":     target => $t, order => 2, content => "has newline\n" }
  concat_fragment { "empty-${i}":  target => $t, order => 3, content => '' }
  concat_fragment { "two-${i}":    target => $t, order => 4, content => "two newlines\n\n" }
  concat_fragment { "inner-${i}":  target => $t, order => 5, content => "inner\nlast" }
}
