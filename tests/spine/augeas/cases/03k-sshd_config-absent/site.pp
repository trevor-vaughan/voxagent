sshd_config { 'PermitRootLogin': ensure => absent }
sshd_config { 'Nothere': ensure => absent }
sshd_config { 'X11Forwarding match': ensure => absent, key => 'X11Forwarding', condition => 'User bob' }
sshd_config { 'UsePrivilegeSeparation': ensure => absent, value => 'sandbox' }
