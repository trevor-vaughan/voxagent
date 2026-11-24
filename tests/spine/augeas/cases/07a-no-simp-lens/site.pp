Ssh_config { target => "/etc/ssh/ssh_config" }
ssh_config { "zzzz98_st____Compression": key => "Compression", host => "*", value => "no" }
ssh_config { "zzzz98_st____Ciphers": key => "Ciphers", host => "*", value => ["aes256-ctr", "aes128-ctr"] }
sshd_config { "PermitRootLogin": value => "no" }
sshd_config { "AllowUsers": value => ["alice", "bob"] }
sshd_config_subsystem { "sftp": command => "/usr/libexec/openssh/sftp-server" }
