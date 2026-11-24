sshd_config { 'AllowUsers': value => ['alice', 'bob'] }
sshd_config { 'Ciphers': value => ['aes256-ctr', 'aes128-ctr'] }
sshd_config { 'AcceptEnv': value => ['LANG', 'LC_*'] }
