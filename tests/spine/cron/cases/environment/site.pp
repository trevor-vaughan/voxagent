cron { 'e1': command => '/bin/e1', minute => '1', environment => ['MAILTO=root', 'PATH=/usr/bin:/bin'] }
cron { 'e2': command => '/bin/e2', minute => '2', environment => 'MAILTO=ops' }
