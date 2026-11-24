file { '/etc/modprobe.d/blacklist.conf': ensure => file, owner => 'root', group => 'root', mode => '0644' }
augeas { 'kmod::setting blacklist cramfs':
  incl    => '/etc/modprobe.d/blacklist.conf',
  lens    => 'Modprobe.lns',
  changes => ["set blacklist[. = 'cramfs'] cramfs"],
  require => File['/etc/modprobe.d/blacklist.conf'],
}
augeas { 'kmod::setting blacklist udf':
  incl    => '/etc/modprobe.d/blacklist.conf',
  lens    => 'Modprobe.lns',
  changes => ["set blacklist[. = 'udf'] udf"],
  require => File['/etc/modprobe.d/blacklist.conf'],
}
