sshd_config { 'PasswordAuthentication match': key => 'PasswordAuthentication', value => 'no', condition => 'Host example.net User root' }
sshd_config { 'X11Forwarding match': key => 'X11Forwarding', value => 'yes', condition => 'User bob' }
sshd_config { 'Banner match': key => 'Banner', value => '/etc/issue', condition => 'User bob' }
