sshkey { 'foo': type => rsa, key => 'AAAA' }
sshkey { 'bar@rsa': key => 'BBBB' }
sshkey { 'baz': type => 'ssh-ed25519', key => 'CCCC' }
