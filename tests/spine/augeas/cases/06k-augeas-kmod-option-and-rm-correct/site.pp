file { '/etc/modprobe.d/blacklist.conf': ensure => file, mode => '0644' }
augeas { 'kmod option':
  incl    => '/etc/modprobe.d/blacklist.conf',
  lens    => 'Modprobe.lns',
  changes => ["set options[. = 'tipc'] tipc", "set options[. = 'tipc']/foo bar"],
  require => File['/etc/modprobe.d/blacklist.conf'],
}
augeas { 'kmod rm':
  incl    => '/etc/modprobe.d/blacklist.conf',
  lens    => 'Modprobe.lns',
  changes => "rm blacklist[. = 'udf']",
  require => Augeas['kmod option'],
}
