# pupmod::agent::cron and swap's shapes: absent by name, one with an entry.
cron { ['puppetd', 'puppetagent']: ensure => 'absent' }
cron { 'dynamic_swappiness': ensure => absent }
