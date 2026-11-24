useradd -m alice
mkdir -m 700 /home/alice/.ssh
cat > /home/alice/.ssh/authorized_keys <<'X'
1024 35 123456789012345678901234567890 legacy@host
from="1.2.3.4" 1024 35 99999 legacy2@host
X
chown -R alice:alice /home/alice/.ssh
