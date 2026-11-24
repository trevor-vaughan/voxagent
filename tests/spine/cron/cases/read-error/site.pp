cron { 'r': command => '/bin/r', target => '1', minute => '1' }
cron { 'r-absent': ensure => absent, target => '1' }
cron { 'ok': command => '/bin/ok', minute => '1' }
