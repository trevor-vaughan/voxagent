file { '/etc/modprobe.d/blacklist.conf': ensure => file, mode => '0644' }
$f = '/etc/modprobe.d/blacklist.conf'
Augeas { incl => $f, lens => 'Modprobe.lns', require => File[$f] }
augeas { 'e1 set bad path':      changes => ["set blacklist[ x"] }
