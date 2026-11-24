# M14 with the declared File titled otherwise: its `path` is the concat path,
# so the Concat_file finds it by that alias and only sets its content.
file { 'declared-by-path': path => '/etc/cx2.conf', ensure => file, owner => 'daemon', mode => '0600' }
concat_file { '/etc/cx2.conf': owner => 'root', mode => '0644' }
concat_fragment { 'fx2': target => '/etc/cx2.conf', content => "from fragment\n" }
