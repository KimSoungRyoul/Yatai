package services

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"text/template"
	"time"

	"github.com/iancoleman/strcase"
	apiv1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/minio/minio-go/v7/pkg/credentials"

	"github.com/minio/minio-go/v7"

	"github.com/bentoml/yatai/schemas/modelschemas"

	"github.com/pkg/errors"
	"gorm.io/gorm"

	"github.com/bentoml/yatai/api-server/models"
	"github.com/bentoml/yatai/common/consts"
)

// nolint:gosec
var awsSecretTemplate = `
[default]
aws_access_key_id = {{.AccessKeyId}}
aws_secret_access_key = {{.SecretAccessKey}}
`

type bentoVersionService struct{}

var BentoVersionService = bentoVersionService{}

func (s *bentoVersionService) getBaseDB(ctx context.Context) *gorm.DB {
	return mustGetSession(ctx).Model(&models.BentoVersion{})
}

type CreateBentoVersionOption struct {
	CreatorId   uint
	BentoId     uint
	Version     string
	Description string
	BuildAt     time.Time
	Manifest    *modelschemas.BentoVersionManifestSchema
}

type UpdateBentoVersionOption struct {
	BuildStatus          *modelschemas.BentoVersionBuildStatus
	UploadStatus         *modelschemas.BentoVersionUploadStatus
	UploadStartedAt      **time.Time
	UploadFinishedAt     **time.Time
	UploadFinishedReason *string
}

type ListBentoVersionOption struct {
	BaseListOption
	BentoId *uint
}

func (s *bentoVersionService) Create(ctx context.Context, opt CreateBentoVersionOption) (bentoVersion *models.BentoVersion, url *url.URL, err error) {
	// nolint: ineffassign,staticcheck
	db, ctx, df, err := startTransaction(ctx)
	if err != nil {
		return
	}
	defer func() { df(err) }()
	bentoVersion = &models.BentoVersion{
		CreatorAssociate: models.CreatorAssociate{
			CreatorId: opt.CreatorId,
		},
		BentoAssociate: models.BentoAssociate{
			BentoId: opt.BentoId,
		},
		Version:      opt.Version,
		Description:  opt.Description,
		BuildStatus:  modelschemas.BentoVersionBuildStatusPending,
		UploadStatus: modelschemas.BentoVersionUploadStatusPending,
		BuildAt:      opt.BuildAt,
		Manifest:     opt.Manifest,
	}
	err = db.Create(bentoVersion).Error
	if err != nil {
		return
	}
	bento, err := BentoService.Get(ctx, opt.BentoId)
	if err != nil {
		return
	}
	org, err := OrganizationService.GetAssociatedOrganization(ctx, bento)
	if err != nil {
		return
	}
	if org.Config == nil {
		err = errors.New("This organization does not have configuration")
		return
	}
	if org.Config.AWS == nil || org.Config.AWS.S3 == nil {
		err = errors.New("This organization does not have aws s3 storage set up")
		return
	}
	minioConf := org.Config.AWS.S3
	minioClient, err := minio.New("s3.amazonaws.com", &minio.Options{
		Creds:  credentials.NewStaticV4(org.Config.AWS.AccessKeyId, org.Config.AWS.SecretAccessKey, ""),
		Secure: true,
	})
	if err != nil {
		err = errors.Wrap(err, "create s3 client")
	}

	bucketName := minioConf.BucketName

	err = minioClient.MakeBucket(ctx, bucketName, minio.MakeBucketOptions{Region: minioConf.Region})
	if err != nil {
		// Check to see if we already own this bucket (which happens if you run this twice)
		exists, errBucketExists := minioClient.BucketExists(ctx, bucketName)
		if errBucketExists != nil || !exists {
			err = errors.Wrapf(err, "create bucket %s", bucketName)
			return
		}
	}

	objectName, err := s.getS3ObjectName(ctx, bentoVersion)
	if err != nil {
		return
	}

	url, err = minioClient.PresignedPutObject(ctx, bucketName, objectName, time.Hour)
	if err != nil {
		err = errors.Wrap(err, "presigned put object")
		return
	}
	return
}

func (s *bentoVersionService) getS3ObjectName(ctx context.Context, bentoVersion *models.BentoVersion) (string, error) {
	bento, err := BentoService.GetAssociatedBento(ctx, bentoVersion)
	if err != nil {
		return "", err
	}
	org, err := OrganizationService.GetAssociatedOrganization(ctx, bento)
	if err != nil {
		return "", err
	}
	objectName := fmt.Sprintf("bentos/%s/%s/%s.tar.gz", org.Name, bento.Name, bentoVersion.Version)
	return objectName, nil
}

func (s *bentoVersionService) GetImageName(ctx context.Context, bentoVersion *models.BentoVersion) (string, error) {
	bento, err := BentoService.GetAssociatedBento(ctx, bentoVersion)
	if err != nil {
		return "", nil
	}
	org, err := OrganizationService.GetAssociatedOrganization(ctx, bento)
	if err != nil {
		return "", nil
	}
	if org.Config == nil || org.Config.AWS == nil || org.Config.AWS.ECR == nil {
		return "", errors.Errorf("organization %s don't have ECR config", org.Name)
	}
	imageName := fmt.Sprintf("%s:yatai.%s.%s.%s", org.Config.AWS.ECR.RepositoryURI, org.Name, bento.Name, bentoVersion.Version)
	return imageName, nil
}

func (s *bentoVersionService) Update(ctx context.Context, bentoVersion *models.BentoVersion, opt UpdateBentoVersionOption) (*models.BentoVersion, error) {
	var err error
	updaters := make(map[string]interface{})
	if opt.BuildStatus != nil {
		updaters["build_status"] = *opt.BuildStatus
		defer func() {
			if err == nil {
				bentoVersion.BuildStatus = *opt.BuildStatus
			}
		}()
	}
	if opt.UploadStatus != nil {
		updaters["upload_status"] = *opt.UploadStatus
		defer func() {
			if err == nil {
				bentoVersion.UploadStatus = *opt.UploadStatus
			}
		}()
	}
	if opt.UploadStartedAt != nil {
		updaters["upload_started_at"] = *opt.UploadStartedAt
		defer func() {
			if err == nil {
				bentoVersion.UploadStartedAt = *opt.UploadStartedAt
			}
		}()
	}
	if opt.UploadFinishedAt != nil {
		updaters["upload_finished_at"] = *opt.UploadFinishedAt
		defer func() {
			if err == nil {
				bentoVersion.UploadFinishedAt = *opt.UploadFinishedAt
			}
		}()
	}
	if opt.UploadFinishedReason != nil {
		updaters["upload_finished_reason"] = *opt.UploadFinishedReason
		defer func() {
			if err == nil {
				bentoVersion.UploadFinishedReason = *opt.UploadFinishedReason
			}
		}()
	}

	if len(updaters) == 0 {
		return bentoVersion, nil
	}

	// nolint: ineffassign,staticcheck
	db, ctx, df, err := startTransaction(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { df(err) }()

	err = db.Model(&models.BentoVersion{}).Where("id = ?", bentoVersion.ID).Updates(updaters).Error
	if err != nil {
		return nil, err
	}

	if opt.UploadStatus == nil || *opt.UploadStatus != modelschemas.BentoVersionUploadStatusSuccess {
		return bentoVersion, err
	}

	bento, err := BentoService.GetAssociatedBento(ctx, bentoVersion)
	if err != nil {
		return nil, err
	}

	org, err := OrganizationService.GetAssociatedOrganization(ctx, bento)
	if err != nil {
		return nil, err
	}

	majorCluster, err := OrganizationService.GetMajorCluster(ctx, org)
	if err != nil {
		return nil, err
	}

	kubeCli, _, err := ClusterService.GetKubeCliSet(ctx, majorCluster)
	if err != nil {
		return nil, err
	}

	kubeNamespace := "yatai-builders"

	_, err = kubeCli.CoreV1().Namespaces().Get(ctx, kubeNamespace, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		_, err = kubeCli.CoreV1().Namespaces().Create(ctx, &apiv1.Namespace{ObjectMeta: metav1.ObjectMeta{
			Name: kubeNamespace,
		}}, metav1.CreateOptions{})
		if err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	}

	if org.Config == nil || org.Config.AWS == nil {
		return nil, errors.Errorf("organization %s don't have aws config", org.Name)
	}

	awsSecretKubeName := "aws-secret"
	var awsSecretBuffer bytes.Buffer
	t := template.Must(template.New(awsSecretKubeName).Parse(awsSecretTemplate))
	if err := t.Execute(&awsSecretBuffer, map[string]string{
		"AccessKeyId":     org.Config.AWS.AccessKeyId,
		"SecretAccessKey": org.Config.AWS.SecretAccessKey,
	}); err != nil {
		return nil, err
	}

	secretsCli := kubeCli.CoreV1().Secrets(kubeNamespace)
	_, err = secretsCli.Get(ctx, awsSecretKubeName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		_, err = secretsCli.Create(ctx, &apiv1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: awsSecretKubeName},
			StringData: map[string]string{
				"credentials": awsSecretBuffer.String(),
			},
		}, metav1.CreateOptions{})
		if err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	}

	dockerCMKubeName := "docker-config"
	dockerCMContent, err := json.Marshal(struct {
		CredsStore string `json:"creds_store"`
	}{
		CredsStore: "ecr-login",
	})
	if err != nil {
		return nil, err
	}
	cmsCli := kubeCli.CoreV1().ConfigMaps(kubeNamespace)
	_, err = cmsCli.Get(ctx, dockerCMKubeName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		_, err = cmsCli.Create(ctx, &apiv1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: dockerCMKubeName},
			Data: map[string]string{
				"config.json": string(dockerCMContent),
			},
		}, metav1.CreateOptions{})
	} else if err != nil {
		return nil, err
	}

	podsCli := kubeCli.CoreV1().Pods(kubeNamespace)

	kubeName, err := s.GetImageBuilderKubeName(ctx, bentoVersion)
	if err != nil {
		return nil, err
	}

	if org.Config == nil || org.Config.AWS == nil || org.Config.AWS.S3 == nil {
		return nil, errors.Errorf("origanization %s don't have s3 config", org.Name)
	}

	s3ObjectName, err := s.getS3ObjectName(ctx, bentoVersion)
	if err != nil {
		return nil, err
	}

	imageName, err := s.GetImageName(ctx, bentoVersion)
	if err != nil {
		return nil, err
	}

	_, err = podsCli.Create(ctx, &apiv1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: kubeName,
		},
		Spec: apiv1.PodSpec{
			RestartPolicy: apiv1.RestartPolicyNever,
			Volumes: []apiv1.Volume{
				{
					Name: dockerCMKubeName,
					VolumeSource: apiv1.VolumeSource{
						ConfigMap: &apiv1.ConfigMapVolumeSource{
							LocalObjectReference: apiv1.LocalObjectReference{
								Name: dockerCMKubeName,
							},
						},
					},
				},
				{
					Name: awsSecretKubeName,
					VolumeSource: apiv1.VolumeSource{
						Secret: &apiv1.SecretVolumeSource{
							SecretName: awsSecretKubeName,
						},
					},
				},
			},
			Containers: []apiv1.Container{
				{
					Name:  "builder",
					Image: "gcr.io/kaniko-project/executor:latest",
					Args: []string{
						"--dockerfile=./Dockerfile",
						fmt.Sprintf("--context=s3://%s/%s", org.Config.AWS.S3.BucketName, s3ObjectName),
						fmt.Sprintf("--destination=%s", imageName),
					},
					VolumeMounts: []apiv1.VolumeMount{
						{
							Name:      dockerCMKubeName,
							MountPath: "/kaniko/.docker/",
						},
						{
							Name:      awsSecretKubeName,
							MountPath: "/root/.aws/",
						},
					},
					Env: []apiv1.EnvVar{
						{
							Name:  "AWS_REGION",
							Value: org.Config.AWS.S3.Region,
						},
					},
				},
			},
		},
	}, metav1.CreateOptions{})

	return bentoVersion, err
}

func (s *bentoVersionService) GetImageBuilderKubeName(ctx context.Context, bentoVersion *models.BentoVersion) (string, error) {
	bento, err := BentoService.GetAssociatedBento(ctx, bentoVersion)
	if err != nil {
		return "", err
	}

	org, err := OrganizationService.GetAssociatedOrganization(ctx, bento)
	if err != nil {
		return "", err
	}

	return strings.ReplaceAll(strcase.ToKebab(fmt.Sprintf("yatai-image-builder-%s-%s-%s", org.Name, bento.Name, bentoVersion.Version)), ".", "-"), nil
}

func (s *bentoVersionService) Get(ctx context.Context, id uint) (*models.BentoVersion, error) {
	var bentoVersion models.BentoVersion
	err := getBaseQuery(ctx, s).Where("id = ?", id).First(&bentoVersion).Error
	if err != nil {
		return nil, err
	}
	if bentoVersion.ID == 0 {
		return nil, consts.ErrNotFound
	}
	return &bentoVersion, nil
}

func (s *bentoVersionService) GetByVersion(ctx context.Context, bentoId uint, version string) (*models.BentoVersion, error) {
	var bentoVersion models.BentoVersion
	err := getBaseQuery(ctx, s).Where("bento_id = ?", bentoId).Where("version = ?", version).First(&bentoVersion).Error
	if err != nil {
		return nil, errors.Wrapf(err, "get bento version %s", version)
	}
	if bentoVersion.ID == 0 {
		return nil, consts.ErrNotFound
	}
	return &bentoVersion, nil
}

func (s *bentoVersionService) List(ctx context.Context, opt ListBentoVersionOption) ([]*models.BentoVersion, uint, error) {
	query := getBaseQuery(ctx, s)
	if opt.BentoId != nil {
		query = query.Where("bento_id = ?", *opt.BentoId)
	}
	var total int64
	err := query.Count(&total).Error
	if err != nil {
		return nil, 0, err
	}
	bentoVersions := make([]*models.BentoVersion, 0)
	query = opt.BindQuery(query).Order("build_at DESC")
	err = query.Find(&bentoVersions).Error
	if err != nil {
		return nil, 0, err
	}
	return bentoVersions, uint(total), err
}

func (s *bentoVersionService) ListLatestByBentoIds(ctx context.Context, bentoIds []uint) ([]*models.BentoVersion, error) {
	db := mustGetSession(ctx)

	query := db.Raw(`select * from bento_version where id in (
					select n.version_id from (
						select bento_id, max(id) as version_id from bento_version 
						where bento_id in (?) group by bento_id
					) as n)`, bentoIds)

	versions := make([]*models.BentoVersion, 0, len(bentoIds))
	err := query.Find(&versions).Error
	if err != nil {
		return nil, err
	}

	return versions, err
}

type IBentoVersionAssociate interface {
	GetAssociatedBentoVersionId() uint
	GetAssociatedBentoVersionCache() *models.BentoVersion
	SetAssociatedBentoVersionCache(version *models.BentoVersion)
}

func (s *bentoVersionService) GetAssociatedBentoVersion(ctx context.Context, associate IBentoVersionAssociate) (*models.BentoVersion, error) {
	cache := associate.GetAssociatedBentoVersionCache()
	if cache != nil {
		return cache, nil
	}
	version, err := s.Get(ctx, associate.GetAssociatedBentoVersionId())
	associate.SetAssociatedBentoVersionCache(version)
	return version, err
}
