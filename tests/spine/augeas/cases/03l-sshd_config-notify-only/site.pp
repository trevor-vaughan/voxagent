# SIMP shape: no value and no ensure -> manages nothing
sshd_config { 'AllowUsers': }
sshd_config { 'PermitRootLogin': value => 'no' }
