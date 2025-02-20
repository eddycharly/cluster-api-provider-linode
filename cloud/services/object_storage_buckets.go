package services

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	corev1 "k8s.io/api/core/v1"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"

	"github.com/linode/linodego"

	"github.com/linode/cluster-api-provider-linode/cloud/scope"
	"github.com/linode/cluster-api-provider-linode/util"
)

func EnsureObjectStorageBucket(ctx context.Context, bScope *scope.ObjectStorageBucketScope) (*linodego.ObjectStorageBucket, error) {
	// Buckets do not have IDs so hardcode it to 0
	listFilter := util.Filter{
		ID:    nil,
		Label: *bScope.Bucket.Spec.Label,
		Tags:  nil,
	}
	buckets, err := bScope.LinodeClient.ListObjectStorageBucketsInCluster(
		ctx,
		linodego.NewListOptions(1, listFilter.String()),
		bScope.Bucket.Spec.Cluster,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to list buckets in cluster %s: %w", bScope.Bucket.Spec.Cluster, err)
	}
	if len(buckets) == 1 {
		bScope.Logger.Info("Bucket exists")

		return &buckets[0], nil
	}

	opts := linodego.ObjectStorageBucketCreateOptions{
		Cluster: bScope.Bucket.Spec.Cluster,
		Label:   *bScope.Bucket.Spec.Label,
		ACL:     linodego.ACLPrivate,
	}

	var bucket *linodego.ObjectStorageBucket
	if bucket, err = bScope.LinodeClient.CreateObjectStorageBucket(ctx, opts); err != nil {
		return nil, fmt.Errorf("failed to create bucket: %w", err)
	}

	bScope.Logger.Info("Created bucket")

	return bucket, nil
}

func RotateObjectStorageKeys(ctx context.Context, bScope *scope.ObjectStorageBucketScope) ([scope.NumAccessKeys]linodego.ObjectStorageKey, error) {
	var newKeys [scope.NumAccessKeys]linodego.ObjectStorageKey

	for idx, permission := range []struct {
		name   string
		suffix string
	}{
		{"read_write", "rw"},
		{"read_only", "ro"},
	} {
		keyLabel := fmt.Sprintf("%s-%s", *bScope.Bucket.Spec.Label, permission.suffix)
		key, err := createObjectStorageKey(ctx, bScope, keyLabel, permission.name)
		if err != nil {
			return newKeys, err
		}

		newKeys[idx] = *key
	}

	// If key revocation fails here, just log the errors since new keys have been created
	if bScope.Bucket.Status.LastKeyGeneration != nil && bScope.ShouldRotateKeys() {
		secret, err := bScope.GetAccessKeySecret(ctx)
		if err != nil {
			bScope.Logger.Error(err, "Failed to read secret with access keys to revoke; keys must be manually revoked")
		}

		if err := RevokeObjectStorageKeys(ctx, bScope, secret); err != nil {
			bScope.Logger.Error(err, "Failed to revoke access keys; keys must be manually revoked")
		}
	}

	return newKeys, nil
}

func createObjectStorageKey(ctx context.Context, bScope *scope.ObjectStorageBucketScope, label, permission string) (*linodego.ObjectStorageKey, error) {
	opts := linodego.ObjectStorageKeyCreateOptions{
		Label: label,
		BucketAccess: &[]linodego.ObjectStorageKeyBucketAccess{
			{
				BucketName:  *bScope.Bucket.Spec.Label,
				Cluster:     bScope.Bucket.Spec.Cluster,
				Permissions: permission,
			},
		},
	}

	key, err := bScope.LinodeClient.CreateObjectStorageKey(ctx, opts)
	if err != nil {
		bScope.Logger.Error(err, "Failed to create access key", "label", label)

		return nil, fmt.Errorf("failed to create access key: %w", err)
	}

	bScope.Logger.Info("Created access key", "id", key.ID)

	return key, nil
}

func RevokeObjectStorageKeys(ctx context.Context, bScope *scope.ObjectStorageBucketScope, secret *corev1.Secret) error {
	if secret == nil {
		return errors.New("unable to read access keys from nil secret")
	}

	keyIDs, err := bScope.GetAccessKeysFromSecret(ctx, secret)
	if err != nil {
		bScope.Logger.Error(err, "Failed to read secret with access keys to revoke; must be manually revoked")

		return fmt.Errorf("failed to read secret %s with access keys: %w", secret.Name, err)
	}

	var errs []error
	for _, keyID := range keyIDs {
		if err := revokeObjectStorageKey(ctx, bScope, keyID); err != nil {
			errs = append(errs, err)
		}
	}

	return utilerrors.NewAggregate(errs)
}

func revokeObjectStorageKey(ctx context.Context, bScope *scope.ObjectStorageBucketScope, keyID int) error {
	if err := bScope.LinodeClient.DeleteObjectStorageKey(ctx, keyID); err != nil {
		linodeErr := &linodego.Error{}
		if errors.As(err, linodeErr) && linodeErr.StatusCode() != http.StatusNotFound {
			bScope.Logger.Error(err, "Failed to revoke access key", "id", keyID)

			return fmt.Errorf("failed to revoke access key: %w", err)
		}
	}

	bScope.Logger.Info("Revoked access key", "id", keyID)

	return nil
}
