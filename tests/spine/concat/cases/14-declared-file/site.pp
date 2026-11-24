file { '/etc/c14a.conf': ensure => file, content => "declared\n", owner => 'daemon', mode => '0600' }
concat_file { '/etc/c14a.conf': owner => 'root', mode => '0644' }
concat_fragment { 'fa': target => '/etc/c14a.conf', content => "from fragment\n" }
file { '/etc/c14b.conf': ensure => absent }
concat_file { '/etc/c14b.conf': }
concat_fragment { 'fb': target => '/etc/c14b.conf', content => "from fragment\n" }
file { '/etc/c14c.conf': ensure => file, content => "declared\n", notify => Notify['file-notifies'] }
concat_file { '/etc/c14c.conf': }
concat_fragment { 'fc': target => '/etc/c14c.conf', content => "from fragment\n" }
notify { 'file-notifies': message => 'notified by declared file', }
