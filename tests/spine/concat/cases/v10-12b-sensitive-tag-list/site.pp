# Sensitive fragment reached only through a list tag.
concat_file { '/etc/c12b.conf': tag => ['s1', 's2'] }
concat_fragment { 'plain':  tag => 's1', order => 1, content => "public\n" }
concat_fragment { 'secret': tag => 's2', order => 2, content => Sensitive("s3cret-token\n") }
