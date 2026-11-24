sshkey { 'foo': type => rsa, ensure => absent }
sshkey { 'bar@rsa': ensure => absent }
sshkey { 'nothere': type => rsa, ensure => absent }
