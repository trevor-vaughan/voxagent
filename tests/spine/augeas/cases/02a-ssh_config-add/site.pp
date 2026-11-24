Ssh_config { target => '/etc/ssh/ssh_config' }
ssh_config { 'zzzz98_st____Compression': key => 'Compression', host => '*', value => 'no' }
ssh_config { 'zzzz98_st____GSSAPIAuthentication': key => 'GSSAPIAuthentication', host => '*', value => 'yes' }
ssh_config { 'zzzz98_st____ServerAliveInterval': key => 'ServerAliveInterval', host => '*', value => '300' }
