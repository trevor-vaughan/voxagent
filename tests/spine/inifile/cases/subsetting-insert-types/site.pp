ini_subsetting { 'start':  path => '/etc/m.ini', section => 'a', setting => 's', subsetting => 'N', value => '1', insert_type => start }
ini_subsetting { 'end':    path => '/etc/m.ini', section => 'a', setting => 'e', subsetting => 'N', value => '1', insert_type => end }
ini_subsetting { 'before': path => '/etc/m.ini', section => 'a', setting => 'b', subsetting => 'N', value => '1', insert_type => before, insert_value => 'Y' }
ini_subsetting { 'after':  path => '/etc/m.ini', section => 'a', setting => 'f', subsetting => 'N', value => '1', insert_type => after, insert_value => 'X' }
ini_subsetting { 'index':  path => '/etc/m.ini', section => 'a', setting => 'i', subsetting => 'N', value => '1', insert_type => index, insert_value => '1' }
