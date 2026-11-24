file { '/etc/modprobe.d/blacklist.conf': ensure => file, mode => '0644' }
$f = '/etc/modprobe.d/blacklist.conf'
Augeas { incl => $f, lens => 'Modprobe.lns', require => File[$f] }
augeas { 'blacklist cramfs':
  changes => ["set blacklist[. = 'cramfs'] cramfs"],
}
notify { 'tell augeas': notify => Augeas['blacklist cramfs'] }
