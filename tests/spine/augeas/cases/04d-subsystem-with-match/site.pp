sshd_config_subsystem { 'sftp': command => '/usr/libexec/openssh/sftp-server' }
sshd_config_subsystem { 'other': command => '/bin/other' }
