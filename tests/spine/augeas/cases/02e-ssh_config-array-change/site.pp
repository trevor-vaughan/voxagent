Ssh_config { target => '/etc/ssh/ssh_config' }
ssh_config { 'zzzz98_st____Ciphers': key => 'Ciphers', host => '*', value => ['aes256-ctr', 'aes128-ctr'] }
ssh_config { 'zzzz98_st____MACs': key => 'MACs', host => '*', value => ['hmac-sha2-512', 'hmac-sha2-256'] }
ssh_config { 'zzzz98_st____SendEnv': key => 'SendEnv', host => '*', value => ['LANG', 'LC_*'] }
