import type { Schemas } from '~/api/client'
import type { JobStatus } from '~/components/app/status-icon'

type Backup = Schemas['Backup']

/** The status icon of a backup. A failed S3 upload fails the backup, even with the file on this server. */
export function backupStatus(backup: Backup): JobStatus {
  switch (backup.status) {
    case 'done':
    case 'uploaded':
      return 'succeeded'
    case 'running':
    case 'uploading':
      return 'running'
    case 'failed':
      return 'failed'
  }
}

/** Where the backup is: "This server · S3", "This server · uploading to S3", "S3", or "" while it runs. */
export function storedOn(backup: Backup): string {
  const places: string[] = []
  if (backup.available) places.push('This server')
  if (backup.status === 'uploading') places.push('uploading to S3')
  else if (backup.s3_key) places.push('S3')
  else if (backup.status === 'failed' && backup.available) places.push('S3 upload failed')
  return places.join(' · ')
}

