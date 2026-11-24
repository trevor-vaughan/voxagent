mount { "/mnt/a": ensure => defined, device => "/dev/x", fstype => "ext4", options => "defaults", pass => "1\nz z z" }
