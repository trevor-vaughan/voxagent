# Ruby truthiness of a false tag in fragment matching and the orphan warning. (tag => '' fails compile: Invalid tag '')
concat_file { '/etc/c22b.conf': tag => false }
concat_fragment { 'false-tag': target => '/etc/c22-nope.conf', tag => false, content => "f\n" }
