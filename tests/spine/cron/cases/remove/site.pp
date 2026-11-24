cron { 'gone': ensure => absent }
cron { 'kept': command => '/bin/kept', minute => '2' }
