concat_file { '/etc/c12n.conf': }
concat_fragment { 'plain':  target => '/etc/c12n.conf', order => 1, content => "public\n" }
concat_fragment { 'secret': target => '/etc/c12n.conf', order => 2, content => Sensitive("s3cret-token\n") }
