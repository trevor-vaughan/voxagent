# §8 item 2 (M6, 06c apply 3): the lines [] wrote reread with aliases absent,
# so a non-empty list is `defined`.
host { 'h1': ip => '192.0.2.1', host_aliases => ['a'] }
host { 'h2': ip => '192.0.2.2', host_aliases => ['x'], comment => 'c2' }
