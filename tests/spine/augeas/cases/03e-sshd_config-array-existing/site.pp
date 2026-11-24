sshd_config { 'AllowUsers': value => ['alice', 'bob'] }
sshd_config { 'AuthorizedKeysFile': value => ['.ssh/authorized_keys', '/etc/ssh/authorized_keys/%u'] }
sshd_config { 'Ciphers': value => ['aes256-ctr', 'aes128-ctr'] }
