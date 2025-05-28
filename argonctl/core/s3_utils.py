"""
S3 utility functions for mongodump archives.
"""
import boto3
import os
from botocore.exceptions import ClientError
from botocore.config import Config

# Load environment variables for AWS configuration
AWS_ACCESS_KEY_ID = os.getenv('AWS_ACCESS_KEY_ID')
AWS_SECRET_ACCESS_KEY = os.getenv('AWS_SECRET_ACCESS_KEY')
# Support both AWS_REGION and AWS_DEFAULT_REGION for compatibility
AWS_REGION = os.getenv('AWS_REGION') or os.getenv('AWS_DEFAULT_REGION', 'us-east-1')
S3_BUCKET = os.getenv('S3_BUCKET')

# Validate required environment variables
if not all([AWS_ACCESS_KEY_ID, AWS_SECRET_ACCESS_KEY, S3_BUCKET]):
    raise ValueError("Missing required AWS environment variables. Please check AWS_ACCESS_KEY_ID, AWS_SECRET_ACCESS_KEY, and S3_BUCKET are set.")

print(f"[DEBUG] S3 Config - Bucket: {S3_BUCKET}, Region: {AWS_REGION}")

# Configure S3 client with retries and timeouts
s3_config = Config(
    region_name=AWS_REGION,
    retries=dict(
        max_attempts=3,
        mode='standard'
    ),
    connect_timeout=5,
    read_timeout=10
)

s3 = boto3.client(
    's3',
    aws_access_key_id=AWS_ACCESS_KEY_ID,
    aws_secret_access_key=AWS_SECRET_ACCESS_KEY,
    config=s3_config
)

def test_s3_connection():
    """Test S3 connection and bucket access, including versioning status."""
    try:
        # Test bucket access
        s3.head_bucket(Bucket=S3_BUCKET)
        
        # Check if versioning is enabled
        versioning = s3.get_bucket_versioning(Bucket=S3_BUCKET)
        if versioning.get('Status') != 'Enabled':
            print(f"[WARN] Bucket versioning not enabled on {S3_BUCKET}. Some features may not work correctly.")
        
        print(f"[INFO] Successfully connected to S3 bucket: {S3_BUCKET} (Region: {AWS_REGION})")
        return True
        
    except ClientError as e:
        error_code = e.response['Error']['Code']
        if error_code == '403':
            print(f"[ERROR] Access denied to S3 bucket {S3_BUCKET}. Check AWS credentials.")
        elif error_code == '404':
            print(f"[ERROR] Bucket {S3_BUCKET} not found.")
        else:
            print(f"[ERROR] S3 test failed: {str(e)}")
        return False
    except Exception as e:
        print(f"[ERROR] Unexpected error testing S3 connection: {str(e)}")
        return False

def upload_to_s3(local_path, s3_path):
    """
    Upload a file to S3 with verification.
    
    Args:
        local_path: Path to the local file to upload
        s3_path: Destination path in S3
        
    Returns:
        str: Version ID if successful, None otherwise
    """
    # Input validation
    if not local_path or not s3_path:
        print("[ERROR] Both local_path and s3_path must be provided")
        return None
        
    # File validation
    try:
        if not os.path.exists(local_path):
            print(f"[ERROR] Local file not found: {local_path}")
            return None
            
        file_size = os.path.getsize(local_path)
        if file_size == 0:
            print(f"[ERROR] File is empty: {local_path}")
            return None
            
        # Test S3 connection before attempting upload
        if not test_s3_connection():
            print("[ERROR] S3 connection test failed, aborting upload")
            return None
            
        print(f"[INFO] Starting upload of {local_path} ({file_size:,} bytes) to s3://{S3_BUCKET}/{s3_path}")
        
        # Perform upload with progress tracking
        with open(local_path, 'rb') as f:
            # Upload with explicit content length to avoid memory issues
            resp = s3.put_object(
                Bucket=S3_BUCKET,
                Key=s3_path,
                Body=f,
                ContentLength=file_size
            )
            
            # Verify upload success
            version_id = resp.get('VersionId')
            if not version_id:
                print("[ERROR] Upload succeeded but no version ID returned. Ensure bucket versioning is enabled.")
                return None
                
            # Verify uploaded file exists and has correct size
            try:
                head = s3.head_object(Bucket=S3_BUCKET, Key=s3_path, VersionId=version_id)
                if head['ContentLength'] != file_size:
                    print(f"[ERROR] Upload size mismatch. Local: {file_size}, S3: {head['ContentLength']}")
                    return None
            except ClientError as e:
                print(f"[ERROR] Failed to verify uploaded file: {str(e)}")
                return None
                
            print(f"[INFO] Successfully uploaded to S3. Version ID: {version_id}")
            return version_id
            
    except ClientError as e:
        error_code = e.response['Error']['Code']
        error_msg = e.response['Error'].get('Message', str(e))
        print(f"[ERROR] S3 upload failed ({error_code}): {error_msg}")
        return None
        
    except IOError as e:
        print(f"[ERROR] Failed to read local file: {str(e)}")
        return None
        
    except Exception as e:
        print(f"[ERROR] Unexpected error in upload_to_s3: {str(e)}")
        return None

def download_from_s3(s3_path, local_path):
    """Download a file from S3."""
    s3.download_file(S3_BUCKET, s3_path, local_path)

# Download a specific version from S3
def download_from_s3_versioned(s3_path, local_path, version_id):
    """Download a specific version of a file from S3."""
    s3.download_file(Bucket=S3_BUCKET, Key=s3_path, Filename=local_path, ExtraArgs={'VersionId': version_id})

def delete_from_s3(s3_path):
    """
    Delete an object and all its versions from S3.
    
    Args:
        s3_path: The path to the object in S3, not including the bucket name
        
    Returns:
        bool: True if deletion was successful, False otherwise
    """
    try:
        # List all versions of the object
        response = s3.list_object_versions(
            Bucket=S3_BUCKET,
            Prefix=s3_path
        )

        # Delete all versions including delete markers
        if 'Versions' in response:
            objects_to_delete = [
                {'Key': version['Key'], 'VersionId': version['VersionId']}
                for version in response['Versions']
            ]

            if 'DeleteMarkers' in response:
                objects_to_delete.extend([
                    {'Key': marker['Key'], 'VersionId': marker['VersionId']}
                    for marker in response['DeleteMarkers']
                ])

            if objects_to_delete:
                s3.delete_objects(
                    Bucket=S3_BUCKET,
                    Delete={'Objects': objects_to_delete}
                )

        return True
    except ClientError as e:
        print(f"[ERROR] Failed to delete S3 object {s3_path}: {str(e)}")
        return False
    except Exception as e:
        print(f"[ERROR] Unexpected error deleting S3 object {s3_path}: {str(e)}")
        return False

def delete_from_s3(s3_path):
    """
    Delete an object and all its versions from S3.
    
    Args:
        s3_path: The path to the object in S3, not including the bucket name
        
    Returns:
        bool: True if deletion was successful, False otherwise
    """
    try:
        print(f"[INFO] Attempting to delete S3 path: {s3_path} from bucket {S3_BUCKET}")
        
        # List all versions of the object
        response = s3.list_object_versions(
            Bucket=S3_BUCKET,
            Prefix=s3_path
        )

        # Delete all versions including delete markers
        versions_deleted = False
        if 'Versions' in response:
            objects_to_delete = [
                {'Key': version['Key'], 'VersionId': version['VersionId']}
                for version in response['Versions']
            ]

            if 'DeleteMarkers' in response:
                objects_to_delete.extend([
                    {'Key': marker['Key'], 'VersionId': marker['VersionId']}
                    for marker in response['DeleteMarkers']
                ])

            if objects_to_delete:
                print(f"[INFO] Deleting {len(objects_to_delete)} objects/versions for {s3_path}")
                s3.delete_objects(
                    Bucket=S3_BUCKET,
                    Delete={'Objects': objects_to_delete}
                )
                versions_deleted = True

        if not versions_deleted:
            # No versions found - try direct deletion
            print(f"[INFO] No versions found, attempting direct deletion of {s3_path}")
            s3.delete_object(
                Bucket=S3_BUCKET,
                Key=s3_path
            )

        return True
    except ClientError as e:
        print(f"[ERROR] Failed to delete S3 object {s3_path}: {str(e)}")
        return False
    except Exception as e:
        print(f"[ERROR] Unexpected error deleting S3 object {s3_path}: {str(e)}")
        return False
