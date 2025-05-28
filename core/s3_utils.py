"""
S3 utility functions for mongodump archives.
"""
import boto3
import os
from dotenv import load_dotenv

load_dotenv(os.path.join(os.path.dirname(__file__), '..', '.env'))

AWS_ACCESS_KEY_ID = os.getenv('AWS_ACCESS_KEY_ID')
AWS_SECRET_ACCESS_KEY = os.getenv('AWS_SECRET_ACCESS_KEY')
AWS_REGION = os.getenv('AWS_REGION', 'us-east-1') # Changed from AWS_DEFAULT_REGION to match .env.example more closely
S3_BUCKET = os.getenv('S3_BUCKET') # Changed from ARGON_S3_BUCKET

s3 = boto3.client(
    's3',
    aws_access_key_id=AWS_ACCESS_KEY_ID,
    aws_secret_access_key=AWS_SECRET_ACCESS_KEY,
    region_name=AWS_REGION # Ensure this uses the loaded AWS_REGION
)

def upload_to_s3(local_path, s3_path):
    """Upload a file to S3. Returns the version ID."""
    # s3.upload_file(local_path, S3_BUCKET, s3_path) # This call is redundant
    # For versioning, use put_object:
    with open(local_path, 'rb') as f:
        resp = s3.put_object(Bucket=S3_BUCKET, Key=s3_path, Body=f)
        version_id = resp.get('VersionId')
        # print(f"Uploaded {s3_path} to S3 bucket {S3_BUCKET}. Version ID: {version_id}") # Optional: for debugging
        return version_id

def download_from_s3(s3_path, local_path):
    """Download a file from S3."""
    s3.download_file(S3_BUCKET, s3_path, local_path)

# Download a specific version from S3
def download_from_s3_versioned(s3_path, local_path, version_id):
    """Download a specific version of a file from S3."""
    s3.download_file(Bucket=S3_BUCKET, Key=s3_path, Filename=local_path, ExtraArgs={'VersionId': version_id})
