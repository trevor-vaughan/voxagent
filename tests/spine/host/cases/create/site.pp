# M6: a full line, a line with ip only, and `a.` (valid: §8 item 4).
host { 'new1.example': ip => '192.0.2.1', host_aliases => ['n1', 'new1'], comment => 'first' }
host { 'new2.example': ip => '192.0.2.2' }
host { 'a.': ip => '192.0.2.3' }
