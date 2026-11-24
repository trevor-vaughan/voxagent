Ssh_config { target => '/etc/ssh/ssh_config' }
ssh_config { 'zzzz98_st____CompressionLevel': ensure => absent, key => 'CompressionLevel', host => '*', value => '6' }
ssh_config { 'zzzz98_st____ForwardX11': ensure => absent, key => 'ForwardX11', host => '*' }
ssh_config { 'zzzz98_st____Nothere': ensure => absent, key => 'Nothere', host => '*' }
