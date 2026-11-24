file { '/etc/modprobe.d/blacklist.conf': ensure => file, mode => '0644' }
$f = '/etc/modprobe.d/blacklist.conf'
Augeas { incl => $f, lens => 'Modprobe.lns', require => File[$f] }
augeas { 'e2 rm bad path':       changes => ["rm blacklist[. = "] }
augeas { 'e6 one token':         changes => ["rm"] }
augeas { 'e7 insert bad where':  changes => ["insert foo sideways blacklist"] }
augeas { 'e8 rm no match ok':    changes => ["rm nothere"] }
augeas { 'e9 set multi-match':   changes => ["set blacklist 'x'"] }
augeas { 'e10 context-less abs bad path': changes => ["rm /files/etc/modprobe.d/blacklist.conf/blacklist[position() = ]"] }
augeas { 'e12 bad lens name': lens => 'Nonesuch.lns', changes => ["set foo bar"] }
augeas { 'e13 mv bad': changes => ["mv nothere other"] }
augeas { 'e14 insert no match': changes => ["insert foo after nothere"] }
augeas { 'e15 rm unmatched [': changes => ["rm blacklist["] }
