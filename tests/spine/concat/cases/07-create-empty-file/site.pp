concat_file { '/etc/c07-false-absent.conf':  create_empty_file => false }
concat_file { '/etc/c07-false-present.conf': create_empty_file => false }
concat_file { '/etc/c07-false-empty.conf':   create_empty_file => false }
concat_file { '/etc/c07-true-absent.conf':   create_empty_file => true }
concat_file { '/etc/c07-true-present.conf':  create_empty_file => true }
# fragment whose content is the empty string: content empty, not nil
concat_file { '/etc/c07-false-emptyfrag.conf': create_empty_file => false }
concat_fragment { 'e': target => '/etc/c07-false-emptyfrag.conf', content => '' }
