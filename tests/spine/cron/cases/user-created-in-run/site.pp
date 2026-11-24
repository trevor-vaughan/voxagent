# M11: alice exists only once User[alice] has run; ghost never does.
user { 'alice': ensure => present, managehome => false }
cron { 'a': command => '/bin/a', user => 'alice', minute => '1' }
cron { 'g': command => '/bin/g', user => 'ghost', minute => '1' }
