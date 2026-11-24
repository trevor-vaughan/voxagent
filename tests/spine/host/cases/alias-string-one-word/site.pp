# M6 (06h): a one-word string is one alias.
host { 's.example': ip => '192.0.2.7', host_aliases => 'single' }
