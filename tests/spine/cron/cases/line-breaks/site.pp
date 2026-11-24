# I-38 (M13): each value on its own target, so cronie's refusal of one
# cannot hide another.
cron { 'lb-minute': command => '/bin/lb', minute => "1\n2" }
cron { 'lb-command': command => "true\n# x", minute => '1', user => 'bin' }
cron { 'lb-env': command => '/bin/lb', environment => ["A=1\nB=2"], minute => '1', user => 'daemon' }
