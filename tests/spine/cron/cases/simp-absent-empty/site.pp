# pupmod::agent::cron and swap's shapes: absent by name, no crontab at all.
cron { ['puppetd', 'puppetagent']: ensure => 'absent' }
cron { 'dynamic_swappiness': ensure => absent }
