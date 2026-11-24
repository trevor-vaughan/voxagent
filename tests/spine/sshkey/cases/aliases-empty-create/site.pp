sshkey { 'h1': type => 'ssh-rsa', key => 'AAAA', host_aliases => [] }
sshkey { 'h2': type => 'ssh-rsa', key => 'BBBB', host_aliases => [], target => '/tmp/kh' }
