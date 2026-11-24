# List tag on concat_file (10.0.1 feature) plus a scalar tag.
concat_file { '/etc/c08l.conf': tag => ['a', 'b'] }
concat_fragment { 'frag-a':   tag => 'a', order => 1, content => "from-a\n" }
concat_fragment { 'frag-b':   tag => 'b', order => 2, content => "from-b\n" }
concat_fragment { 'frag-c':   tag => 'c', order => 3, content => "from-c-not-listed\n" }
concat_fragment { 'frag-tgt': target => '/etc/c08l.conf', order => 4, content => "from-target\n" }
concat_fragment { 'frag-both': target => 'nowhere', tag => 'b', order => 5, content => "from-target-miss-tag-b\n" }
concat_file { '/etc/c08s.conf': tag => 'x' }
concat_fragment { 'frag-x': tag => 'x', order => 1, content => "from-x\n" }
concat_fragment { 'frag-y': tag => 'y', order => 2, content => "from-y-orphan\n" }
