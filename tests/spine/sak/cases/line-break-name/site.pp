ssh_authorized_key { "a\nb": ensure => present, user => "root", type => "ssh-rsa", key => "AAAA" }
