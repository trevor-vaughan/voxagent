# concat_file whose title differs from its path and carries a tag.
concat_file { 'title08': path => '/etc/c08.conf', tag => 'grp08' }
concat_fragment { 'by-path':  target => '/etc/c08.conf', order => 1, content => "by-path\n" }
concat_fragment { 'by-title': target => 'title08',       order => 2, content => "by-title\n" }
concat_fragment { 'by-tag':   tag => 'grp08',            order => 3, content => "by-tag\n" }
concat_fragment { 'target-and-tag': target => 'nowhere', tag => 'grp08', order => 4, content => "target-and-tag\n" }
# orphans
concat_fragment { 'orphan-target': target => '/etc/c08-nope.conf', content => "x\n" }
concat_fragment { 'orphan-tag':    tag => 'no-such-tag', content => "x\n" }
concat_fragment { 'orphan-both':   target => '/etc/c08-nope2.conf', tag => 'no-such-tag2', content => "x\n" }
