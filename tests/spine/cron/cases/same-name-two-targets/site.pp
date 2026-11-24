# A catalog cannot hold two Cron named alike (the compiler refuses the alias),
# so the second name lives in root's crontab: Cron[job] binds only to bin's
# record, and root's, rewritten for Cron[other], keeps its own.
cron { 'job': command => '/bin/new', minute => '2', user => 'bin' }
cron { 'other': command => '/bin/other', minute => '3' }
