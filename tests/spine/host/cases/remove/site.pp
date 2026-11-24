# M6 (06a): two removed; never.example is absent already (no event).
host { 'new1.example': ensure => absent }
host { 'new2.example': ensure => absent }
host { 'never.example': ensure => absent }
