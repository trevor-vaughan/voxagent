sshd_config { 'PasswordAuthentication match': key => 'PasswordAuthentication', value => 'no', condition => 'Host example.net User root' }
sshd_config { 'AllowUsers match': key => 'AllowUsers', value => ['alice', 'bob'], condition => 'User carol' }
