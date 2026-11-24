cron { 'j1': command => '/bin/j1', minute => '1', environment => [] }
cron { 'j2': command => '/bin/j2', minute => '2', environment => ['A=1'] }
