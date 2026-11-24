file { '/etc/resolv.conf': owner => 'root', group => 'root', mode => '0644' }
augeas { '/etc/resolv.conf':
  context => '/files/etc/resolv.conf',
  changes => "rm #comment\nrm nameserver[.!=\"10.0.0.53\"]\nset nameserver[1] 10.0.0.53\nrm search/domain[.!=\"example.com\"]\nset search/domain[.=\"example.com\"] example.com\nrm options/debug\nrm options/inet6\nrm options/no-check-names\ndefnode opts options \"\"\nset \$opts/attempts 2\nset \$opts/ndots 1\nset \$opts/rotate \"\"\nset \$opts/timeout 2\nrm options[count(*)=0]\n",
  require => File['/etc/resolv.conf'],
}
