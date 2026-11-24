file { '/etc/modprobe.d/blacklist.conf': ensure => file, mode => '0644' }
$f = '/etc/modprobe.d/blacklist.conf'
Augeas { incl => $f, lens => 'Modprobe.lns', require => File[$f] }
augeas { 'onlyif match size true':
  onlyif  => "match blacklist[. = 'udf'] size == 1",
  changes => ["set blacklist[. = 'cramfs'] cramfs"],
}
