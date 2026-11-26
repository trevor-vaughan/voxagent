# SIMP firewalld's shape: only the old fw_* file goes.
tidy { '/srv/fw': age => '60m', type => 'mtime', matches => ['fw_*'], recurse => true, backup => false }
