file { '/etc/modprobe.d/blacklist.conf': ensure => file, mode => '0644' }
$f = '/etc/modprobe.d/blacklist.conf'
augeas { 'changes as array':
  incl => $f, lens => 'Modprobe.lns', require => File[$f],
  changes => ["set blacklist[. = 'a1'] a1", "set blacklist[. = 'a2'] a2"],
}
augeas { 'changes as string':
  incl => $f, lens => 'Modprobe.lns', require => File[$f],
  changes => "set blacklist[. = 's1'] s1\nset blacklist[. = 's2'] s2",
}
augeas { 'changes as one string with CRLF-free blank lines':
  incl => $f, lens => 'Modprobe.lns', require => File[$f],
  changes => "\n  set blacklist[. = 'b1'] b1\n\n  set blacklist[. = 'b2'] b2\n",
}
augeas { 'changes as nested array':
  incl => $f, lens => 'Modprobe.lns', require => File[$f],
  changes => [["set blacklist[. = 'n1'] n1"], "set blacklist[. = 'n2'] n2"],
}
