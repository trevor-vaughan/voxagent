Ssh_config { target => '/etc/ssh/ssh_config' }
ssh_config { 'zzzz98_st____Compression': key => 'Compression', host => '*', value => 'no' }
ssh_config { 'zzzz98_st____ForwardX11': key => 'ForwardX11', host => '*', value => 'yes' }
ssh_config { 'zzzz98_st____GSSAPIAuthentication': key => 'GSSAPIAuthentication', host => '*', value => 'yes' }
