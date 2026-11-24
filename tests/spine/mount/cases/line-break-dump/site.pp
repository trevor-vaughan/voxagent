mount { "/mnt/a": ensure => defined, device => "/dev/x", fstype => "ext4", options => "defaults", dump => "0\nz z z" }
