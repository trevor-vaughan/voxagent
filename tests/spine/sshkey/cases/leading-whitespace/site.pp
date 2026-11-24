sshkey { 'good': type => rsa, key => 'GGGG2' }
sshkey { 'other': type => rsa, key => 'OOOO' }
file { '/tmp/unrelated': content => "x\n" }
