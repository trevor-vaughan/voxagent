# M6 (06b): an ip change, and the same aliases in another order.
host { 'h1': ip => '192.0.2.99', host_aliases => ['a', 'b', 'c'], comment => 'c1' }
host { 'h2': ip => '192.0.2.2', host_aliases => ['y', 'x'] }
