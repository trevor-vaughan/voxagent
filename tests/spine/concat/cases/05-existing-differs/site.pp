concat_file { '/etc/c05a.conf': }
concat_file { '/etc/c05b.conf': backup => false }
concat_file { '/etc/c05c.conf': backup => '.bak' }
concat_file { '/etc/c05d.conf': show_diff => false }
concat_file { '/etc/c05e.conf': backup => 'puppet', show_diff => true }
['a','b','c','d','e'].each |$x| {
  concat_fragment { "f-${x}": target => "/etc/c05${x}.conf", content => "new ${x}\n" }
}
