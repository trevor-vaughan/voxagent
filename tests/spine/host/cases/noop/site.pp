# M6 (06g): every kind of change; its --noop run is the point.
host { 'h1': ip => '192.0.2.9', host_aliases => ['b'], comment => 'n' }
host { 'h2': ip => '192.0.2.2' }
host { 'localhost': ensure => absent }
