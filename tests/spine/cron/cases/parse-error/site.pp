cron { 'a': command => '/bin/b', minute => '1' }
cron { 'b': command => '/bin/b', user => 'bin', minute => '2' }
