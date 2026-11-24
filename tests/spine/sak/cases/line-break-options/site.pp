ssh_authorized_key { "k1": ensure => present, user => "root", type => "ssh-rsa", key => "AAAA", options => ["no-pty\nfoo=\"bar\""] }
