# §8 item 2 (M6, 06c): [] writes `ip\tname\t`; a host without aliases is in sync.
host { 'h1': ip => '192.0.2.1', host_aliases => [] }
host { 'h2': ip => '192.0.2.2', host_aliases => [], comment => 'c2' }
host { 'h3': ip => '192.0.2.3', host_aliases => [] }
