# Stored credentials and remote backups

The control panel encrypts saved S3 access and secret keys. Integrations shows
**Stored securely** beside each saved credential. Inputs remain empty and show
**Saved credential · leave blank to keep**; entering both keys replaces them.
Email authentication fields use the same presence indicators. No raw saved secret
values are returned by configuration APIs.

A successful storage test writes and deletes a temporary object. It does not
upload database backups. Enable **Also copy to S3-compatible storage** in the
selected database's backup policy and save, then choose **Back up now**. The
operation status updates while work is active. Each retained backup distinguishes
local-only, failed upload and verified S3 copies. Failures preserve the local dump.

**Remote backups → Check bucket** reads real objects from S3 using the released
Līdza storage API. It shows full prefix/key, size and modification time. The list
includes objects retained remotely after local pruning. Downloads require an
administrator and use short-lived signed URLs after checking the object. Remote
files remain private; a public CDN listing or URL is not required or desirable.
Inventory errors preserve previously loaded objects. Bucket listing requires
`ListBucket` permission; connection testing checks writes/deletes and does not
prove listing permission. Reads/downloads need object read permission.

At most 1,000 objects are shown; the UI identifies a potentially truncated list.
The configured bucket/prefix determines the inventory scope. Other destinations
from unrelated configurations are not automatically searched. Use the storage
provider's private bucket browser for the full inventory.

For DigitalOcean Spaces, use the regional endpoint, for example
`https://nyc3.digitaloceanspaces.com`, and enter `kova` separately as the bucket.
Bucket-specific endpoints such as `https://kova.nyc3.digitaloceanspaces.com` are
normalized with the framework's addressing option. Older releases could write
under an extra `kova/` folder. The inventory includes that previous layout and
labels its objects. A listing-permission failure for that layout produces an
explicit partial-inventory warning. The extra-folder location is checked even if you previously corrected the URL.
Saving configuration preserves its marker while the API destination, bucket and
prefix stay the same.

**Upload to S3** retries a retained local backup without dumping the database
again or creating a second local copy. Successful uploads are checked against
remote object size. Missing local files and provider failures are reported; they
never mark a backup as uploaded. Ordinary remote history uses provider lifecycle
rules. The rolling predeployment copy uses a single `predeployment.dump` object.
Restoration currently uses retained local backups; a remote-only object can be
downloaded here, but this page does not yet offer direct remote-only restore.
