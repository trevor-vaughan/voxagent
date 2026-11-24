# Same order 10, names 'a' (String) and '5' (all digits -> Integer): the numeric
# sort compares [10,'a'] with [10,5].
concat_file { '/etc/c03c.conf': }
concat_fragment { 'a': target => '/etc/c03c.conf', order => 10, content => "a\n" }
concat_fragment { '5': target => '/etc/c03c.conf', order => 10, content => "5\n" }
