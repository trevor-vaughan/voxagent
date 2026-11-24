sshd_config { 'PermitRootLogin': value => 'no' }
sshd_config { 'X11Forwarding': value => 'no' }
sshd_config { 'ClientAliveInterval': value => '300' }
sshd_config { 'UsePAM': value => 'yes' }
