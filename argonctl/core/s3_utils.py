import boto3
import os
from botocore.exceptions import ClientError
from botocore.config import Config

# ANSI color codes for log levels
COLOR_INFO = "\033[32m[INFO]\033[0m"
COLOR_WARN = "\033[33m[WARN]\033[0m"
COLOR_ERROR = "\033[31m[ERROR]\033[0m"

# Load environment variables for AWS configuration
# Load .env file if present
try:
    from dotenv import load_dotenv
    load_dotenv()
except ImportError:
    print(f"{COLOR_WARN} python-dotenv not installed, skipping .env loading")
AWS_ACCESS_KEY_ID = os.getenv('AWS_ACCESS_KEY_ID')
AWS_SECRET_ACCESS_KEY = os.getenv('AWS_SECRET_ACCESS_KEY')
AWS_REGION = os.getenv('AWS_REGION') or os.getenv('AWS_DEFAULT_REGION', 'us-east-1')
S3_BUCKET = os.getenv('S3_BUCKET')
print(f"{COLOR_INFO} Loaded S3 config: bucket={S3_BUCKET}, region={AWS_REGION}, access_key_id={'***' if AWS_ACCESS_KEY_ID else None}")

if not all([AWS_ACCESS_KEY_ID, AWS_SECRET_ACCESS_KEY, S3_BUCKET]):
    raise ValueError("Missing required AWS environment variables. Please check AWS_ACCESS_KEY_ID, AWS_SECRET_ACCESS_KEY, and S3_BUCKET are set.")

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
        s3.head_bucket(Bucket=S3_BUCKET)
        versioning = s3.get_bucket_versioning(Bucket=S3_BUCKET)
        if versioning.get('Status') != 'Enabled':
            print(f"{COLOR_WARN} Bucket versioning not enabled on {S3_BUCKET}. Some features may not work correctly.")
        print(f"{COLOR_INFO} Successfully connected to S3 bucket: {S3_BUCKET} (Region: {AWS_REGION})")
        return True
    except ClientError as e:
        error_code = e.response['Error']['Code']
        if error_code == '403':
            print(f"{COLOR_ERROR} Access denied to S3 bucket {S3_BUCKET}. Check AWS credentials.")
        elif error_code == '404':
            print(f"{COLOR_ERROR} Bucket {S3_BUCKET} not found.")
        else:
            print(f"{COLOR_ERROR} S3 test failed: {str(e)}")
        return False
    except Exception as e:
        print(f"{COLOR_ERROR} Unexpected error testing S3 connection: {str(e)}")
        return False

def upload_to_s3(local_path, s3_path):
    """
    Upload a file to S3 with verification.
    """
    if not local_path or not s3_path:
        print(f"{COLOR_ERROR} Both local_path and s3_path must be provided")
        return None
    try:
        if not os.path.exists(local_path):
            print(f"{COLOR_ERROR} Local file not found: {local_path}")
            return None
        file_size = os.path.getsize(local_path)
        if file_size == 0:
            print(f"{COLOR_ERROR} File is empty: {local_path}")
            return None
        if not test_s3_connection():
            print(f"{COLOR_ERROR} S3 connection test failed, aborting upload")
            return None
        print(f"{COLOR_INFO} Starting upload of {local_path} ({file_size:,} bytes) to s3://{S3_BUCKET}/{s3_path}")
        with open(local_path, 'rb') as f:
            resp = s3.put_object(
                Bucket=S3_BUCKET,
                Key=s3_path,
                Body=f,
                ContentLength=file_size
            )
            version_id = resp.get('VersionId')
            if not version_id:
                print(f"{COLOR_ERROR} Upload succeeded but no version ID returned. Ensure bucket versioning is enabled.")
                return None
            try:
                head = s3.head_object(Bucket=S3_BUCKET, Key=s3_path, VersionId=version_id)
                if head['ContentLength'] != file_size:
                    print(f"{COLOR_ERROR} Upload size mismatch. Local: {file_size}, S3: {head['ContentLength']}")
                    return None
            except ClientError as e:
                print(f"{COLOR_ERROR} Failed to verify uploaded file: {str(e)}")
                return None
            print(f"{COLOR_INFO} Successfully uploaded to S3. Version ID: {version_id}")
            return version_id
    except ClientError as e:
        error_code = e.response['Error']['Code']
        error_msg = e.response['Error'].get('Message', str(e))
        print(f"{COLOR_ERROR} S3 upload failed ({error_code}): {error_msg}")
        return None
    except IOError as e:
        print(f"{COLOR_ERROR} Failed to read local file: {str(e)}")
        return None
    except Exception as e:
        print(f"{COLOR_ERROR} Unexpected error in upload_to_s3: {str(e)}")
        return None

def download_from_s3(s3_path, local_path):
    """Download a file from S3."""
    s3.download_file(S3_BUCKET, s3_path, local_path)

def download_from_s3_versioned(s3_path, local_path, version_id):
    """Download a specific version of a file from S3."""
    s3.download_file(Bucket=S3_BUCKET, Key=s3_path, Filename=local_path, ExtraArgs={'VersionId': version_id})

def delete_from_s3(s3_path):
    """
    Delete an object and all its versions from S3.
    """
    try:
        print(f"{COLOR_INFO} Attempting to delete S3 path: {s3_path} from bucket {S3_BUCKET}")
        response = s3.list_object_versions(
            Bucket=S3_BUCKET,
            Prefix=s3_path
        )
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
                print(f"{COLOR_INFO} Deleting {len(objects_to_delete)} objects/versions for {s3_path}")
                s3.delete_objects(
                    Bucket=S3_BUCKET,
                    Delete={'Objects': objects_to_delete}
                )
                versions_deleted = True
        if not versions_deleted:
            print(f"{COLOR_INFO} No versions found, attempting direct deletion of {s3_path}")
            s3.delete_object(
                Bucket=S3_BUCKET,
                Key=s3_path
            )
        return True
    except ClientError as e:
        print(f"{COLOR_ERROR} Failed to delete S3 object {s3_path}: {str(e)}")
        return False
    except Exception as e:
        print(f"{COLOR_ERROR} Unexpected error deleting S3 object {s3_path}: {str(e)}")
        return False
