# Fix Backup Script

The nightly backup script is failing silently on the new NFS mount.

Error: "permission denied: /mnt/backup-v2/daily"

Need to:
- Check NFS mount permissions
- Update the script to use new mount path
- Add proper error notification (email or Slack)
