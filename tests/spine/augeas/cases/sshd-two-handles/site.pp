# Three resources on one file, alternating type: each type has its own handle.
sshd_config { 'PermitRootLogin': value => 'no' }
sshd_config_subsystem { 'sftp': command => '/usr/libexec/openssh/sftp-server -l INFO' }
sshd_config { 'X11Forwarding': value => 'no' }
