# M4 (04h-newtarget), M5 (05h): a missing target is created 0600 with no
# backup; /etc/hosts is backed up once for two flushes.
host { 'h1': ip => '192.0.2.11' }
host { 'h2': ip => '192.0.2.22' }
host { 'm1': ip => '192.0.2.33', target => '/tmp/missing' }
