sshd_config { 'MaxAuthTries': value => '3' }
sshd_config { 'PermitRootLogin': value => 'no' }
sshd_config { 'AllowGroups': value => ['wheel', 'admins'] }
