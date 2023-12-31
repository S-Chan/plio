package integration

import (
	"encoding/json"
	"net/url"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/cloudtrail"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/iam"
	"github.com/aws/aws-sdk-go/service/s3"
)

// AWS checks that the user's AWS infra is SOC2 compliant
type AWS struct {
	IAM        *IAM
	S3         *S3
	VPC        *VPC
	CloudTrail *CloudTrail
}

// New returns a new AWS integration
func NewAWS(region string) (*AWS, error) {
	s := session.Must(session.NewSession(aws.NewConfig().WithRegion(region)))

	ec2API := ec2.New(s)
	regionOut, err := ec2API.DescribeRegions(nil)
	if err != nil {
		return nil, err
	}
	var regions []string
	for _, r := range regionOut.Regions {
		regions = append(regions, aws.StringValue(r.RegionName))
	}

	return &AWS{
		IAM:        NewIAM(s),
		S3:         NewS3(s),
		VPC:        NewVPC(s, regions),
		CloudTrail: NewCloudTrail(s, regions),
	}, nil
}

// Check checks that the user's AWS infra is SOC2 compliant
func (a *AWS) Check() ([]Result, error) {
	iamRes, err := a.IAM.Check()
	if err != nil {
		return nil, err
	}

	s3Res, err := a.S3.Check()
	if err != nil {
		return nil, err
	}

	vpcRes, err := a.VPC.Check()
	if err != nil {
		return nil, err
	}

	cloudTrailRes, err := a.CloudTrail.Check()
	if err != nil {
		return nil, err
	}

	return concatSlice(iamRes, s3Res, vpcRes, cloudTrailRes), nil
}

// IAM checks that the user's IAM infra is SOC2 compliant
type IAM struct {
	iamAPI *iam.IAM
}

// NewIAM returns a new IAM integration
func NewIAM(s *session.Session) *IAM {
	return &IAM{iamAPI: iam.New(s)}
}

// Check checks that the user's IAM infra is SOC2 compliant
func (i *IAM) Check() ([]Result, error) {
	mfaRes, err := i.checkConsoleMFA()
	if err != nil {
		return nil, err
	}

	staleCredsRes, err := i.checkIAMUsersUnusedCreds()
	if err != nil {
		return nil, err
	}

	rootMfaRes, err := i.checkRootAccountMFA()
	if err != nil {
		return nil, err
	}

	rootAccessKeysRes, err := i.checkRootAccountAccessKeys()
	if err != nil {
		return nil, err
	}

	adminPolicyRes, err := i.checkPolicyNoStatementsWithAdminAccess()
	if err != nil {
		return nil, err
	}

	userPolicyRes, err := i.checkNoUserPolicies()
	if err != nil {
		return nil, err
	}

	return concatSlice(
		mfaRes,
		staleCredsRes,
		rootMfaRes,
		rootAccessKeysRes,
		adminPolicyRes,
		userPolicyRes,
	), nil
}

// checkConsoleMFA checks that IAM users with console access have MFA enabled
func (i *IAM) checkConsoleMFA() ([]Result, error) {
	var mfaRes []Result
	rule := "IAM users with console access must have MFA enabled"

	users, err := i.iamAPI.ListUsers(&iam.ListUsersInput{})
	if err != nil {
		return nil, err
	}

	for _, user := range users.Users {
		mfa, err := i.iamAPI.ListMFADevices(&iam.ListMFADevicesInput{UserName: user.UserName})
		if err != nil {
			return nil, err
		}

		_, err = i.iamAPI.GetLoginProfile(&iam.GetLoginProfileInput{UserName: user.UserName})
		if err != nil {
			mfaRes = append(
				mfaRes,
				i.userResult(aws.StringValue(user.Arn), rule, true, "User does not have console access"),
			)
			continue
		}

		if mfa.MFADevices == nil {
			mfaRes = append(
				mfaRes,
				i.userResult(aws.StringValue(user.Arn), rule, false, "User does not have MFA enabled"),
			)
		} else {
			mfaRes = append(mfaRes, i.userResult(aws.StringValue(user.Arn), rule, true, ""))
		}
	}

	return mfaRes, nil
}

// checkIAMUsersUnusedCreds checks that IAM users have no unused credentials
func (i *IAM) checkIAMUsersUnusedCreds() ([]Result, error) {
	var staleCredsRes []Result
	rule := "IAM users must not have credentials unused in the last 90 days"

	users, err := i.iamAPI.ListUsers(&iam.ListUsersInput{})
	if err != nil {
		return nil, err
	}
	for _, user := range users.Users {
		accessKeys, err := i.iamAPI.ListAccessKeys(
			&iam.ListAccessKeysInput{UserName: user.UserName})
		if err != nil {
			return nil, err
		}

		for _, accessKey := range accessKeys.AccessKeyMetadata {
			if aws.StringValue(accessKey.Status) != "Active" {
				continue
			}

			out, err := i.iamAPI.GetAccessKeyLastUsed(
				&iam.GetAccessKeyLastUsedInput{AccessKeyId: accessKey.AccessKeyId})
			if err != nil {
				return nil, err
			}
			if out.AccessKeyLastUsed.LastUsedDate != nil && out.AccessKeyLastUsed.LastUsedDate.AddDate(0, 0, 90).Before(time.Now()) {
				staleCredsRes = append(
					staleCredsRes,
					i.userResult(aws.StringValue(user.Arn), rule, false, "User has credentials unused for more than 90 days"),
				)
			} else {
				staleCredsRes = append(staleCredsRes, i.userResult(aws.StringValue(user.Arn), rule, true, ""))
			}
		}
	}

	return staleCredsRes, nil
}

// checkRootAccountMFA checks that the root account has MFA enabled
func (i *IAM) checkRootAccountMFA() ([]Result, error) {
	rule := "Root account must have MFA enabled"

	root, err := i.iamAPI.GetAccountSummary(&iam.GetAccountSummaryInput{})
	if err != nil {
		return nil, err
	}

	if aws.Int64Value(root.SummaryMap["AccountMFAEnabled"]) == 0 {
		return []Result{i.userResult("root", rule, false, "Root account does not have MFA enabled")}, nil
	}

	return []Result{i.userResult("root", rule, true, "")}, nil
}

// checkRootAccountAccessKeys checks that the root account has no access keys
func (i *IAM) checkRootAccountAccessKeys() ([]Result, error) {
	rule := "Root account must not have access keys"

	root, err := i.iamAPI.GetAccountSummary(&iam.GetAccountSummaryInput{})
	if err != nil {
		return nil, err
	}

	if aws.Int64Value(root.SummaryMap["AccountAccessKeysPresent"]) != 0 {
		return []Result{i.userResult("root", rule, false, "Root account has access keys")}, nil
	}

	return []Result{i.userResult("root", rule, true, "")}, nil
}

// checkPolicyNoStatementsWithAdminAccess checks that there are no policy
// statements with admin access
func (i *IAM) checkPolicyNoStatementsWithAdminAccess() ([]Result, error) {
	var statementsRes []Result
	rule := "IAM policies must not have statements with admin access"

	policies, err := i.iamAPI.ListPolicies(
		&iam.ListPoliciesInput{Scope: aws.String("Local")},
	)
	if err != nil {
		return nil, err
	}

NEXTPOLICY:
	for _, policy := range policies.Policies {
		defaultVer, err := i.iamAPI.GetPolicyVersion(
			&iam.GetPolicyVersionInput{
				PolicyArn: policy.Arn,
				VersionId: policy.DefaultVersionId,
			})
		if err != nil {
			return nil, err
		}

		defaultVerJSON, err := url.QueryUnescape(aws.StringValue(defaultVer.PolicyVersion.Document))
		if err != nil {
			return nil, err
		}

		var policyDoc map[string]interface{}
		err = json.NewDecoder(strings.NewReader(defaultVerJSON)).Decode(&policyDoc)
		if err != nil {
			return nil, err
		}

		statements := policyDoc["Statement"].([]interface{})
		for _, statement := range statements {
			statementMap := statement.(map[string]interface{})
			isEffectAllow := statementMap["Effect"].(string) == "Allow"
			isActionAdmin := statementMap["Action"] == "*"
			isResourceAdmin := statementMap["Resource"] == "*"
			if isEffectAllow && isActionAdmin && isResourceAdmin {
				statementsRes = append(
					statementsRes,
					i.policyResult(aws.StringValue(policy.Arn), rule, false, "Policy has statement with admin access"),
				)
				continue NEXTPOLICY
			}
		}

		statementsRes = append(statementsRes, i.policyResult(aws.StringValue(policy.Arn), rule, true, ""))
	}

	return statementsRes, nil
}

// checkNoUserPolicies checks that no users have policies attached
func (i *IAM) checkNoUserPolicies() ([]Result, error) {
	var userPoliciesRes []Result
	rule := "IAM users must not have policies attached"

	users, err := i.iamAPI.ListUsers(&iam.ListUsersInput{})
	if err != nil {
		return nil, err
	}

	for _, user := range users.Users {
		userPolicies, err := i.iamAPI.ListUserPolicies(
			&iam.ListUserPoliciesInput{UserName: user.UserName})
		if err != nil {
			return nil, err
		}
		if len(userPolicies.PolicyNames) > 0 {
			userPoliciesRes = append(userPoliciesRes, i.userResult(aws.StringValue(user.UserName), rule, false, "User has inline policies attached"))
			continue
		}

		attachedPolicies, err := i.iamAPI.ListAttachedUserPolicies(
			&iam.ListAttachedUserPoliciesInput{UserName: user.UserName})
		if err != nil {
			return nil, err
		}
		if len(attachedPolicies.AttachedPolicies) > 0 {
			userPoliciesRes = append(userPoliciesRes, i.userResult(aws.StringValue(user.UserName), rule, false, "User has managed policies attached"))
			continue
		}

		userPoliciesRes = append(userPoliciesRes, i.userResult(aws.StringValue(user.UserName), rule, true, ""))
	}

	return userPoliciesRes, nil
}

func (i *IAM) userResult(name, rule string, compliant bool, reason string) Result {
	return Result{
		Resource: Resource{
			Type: "aws/iam-user",
			Name: name,
		},
		Rule:      rule,
		Compliant: compliant,
		Reason:    reason,
	}
}

func (i *IAM) policyResult(name, rule string, compliant bool, reason string) Result {
	return Result{
		Resource: Resource{
			Type: "aws/iam-policy",
			Name: name,
		},
		Rule:      rule,
		Compliant: compliant,
		Reason:    reason,
	}
}

// S3 checks that the user's IAM infra is SOC2 compliant
type S3 struct {
	s3API *s3.S3
}

// NewS3 returns a new S3 integration
func NewS3(s *session.Session) *S3 {
	return &S3{s3API: s3.New(s)}
}

// Check checks that the user's S3 infra is SOC2 compliant
func (s *S3) Check() ([]Result, error) {
	return s.checkS3BucketEncryption()
}

// checkS3BucketEncryption checks that S3 buckets are encrypted
func (s *S3) checkS3BucketEncryption() ([]Result, error) {
	var s3Res []Result
	rule := "S3 buckets must be encrypted"

	buckets, err := s.s3API.ListBuckets(nil)
	if err != nil {
		return nil, err
	}

	for _, bucket := range buckets.Buckets {
		bucketLoc, err := s.s3API.GetBucketLocation(&s3.GetBucketLocationInput{Bucket: bucket.Name})
		if err != nil {
			return nil, err
		}

		region := aws.StringValue(bucketLoc.LocationConstraint)
		if len(region) == 0 {
			// Buckets in Region us-east-1 have a LocationConstraint of null.
			region = "us-east-1"
		}

		regionSession := session.Must(
			session.NewSession(
				aws.NewConfig().WithRegion(region)))
		regionS3API := s3.New(regionSession)

		encryption, err := regionS3API.GetBucketEncryption(
			&s3.GetBucketEncryptionInput{Bucket: bucket.Name})
		if err != nil {
			return nil, err
		}

		if encryption.ServerSideEncryptionConfiguration == nil {
			s3Res = append(
				s3Res,
				s.bucketResult(bucket, rule, false, "Bucket is not encrypted"),
			)
		} else {
			s3Res = append(s3Res, s.bucketResult(bucket, rule, true, ""))
		}
	}

	return s3Res, nil
}

func (s *S3) bucketResult(bucket *s3.Bucket, rule string, compliant bool, reason string) Result {
	return Result{
		Resource: Resource{
			Type: "aws/s3-bucket",
			Name: aws.StringValue(bucket.Name),
		},
		Rule:      rule,
		Compliant: compliant,
		Reason:    reason,
	}
}

// VPC checks that the user's VPCs are SOC2 compliant
type VPC struct {
	ec2API  *ec2.EC2
	regions []string
}

// NewVPC returns a new VPC integration
func NewVPC(s *session.Session, regions []string) *VPC {
	return &VPC{ec2API: ec2.New(s), regions: regions}
}

// Check checks that the user's VPCs are SOC2 compliant
func (v *VPC) Check() ([]Result, error) {
	flowLogsRes, err := v.checkVPCFlowLogs()
	if err != nil {
		return nil, err
	}

	vpcDefaultSGRes, err := v.checkVPCDefaultSecurityGroup()
	if err != nil {
		return nil, err
	}

	restrictedSSH, err := v.checkRestrictedSSH()
	if err != nil {
		return nil, err
	}

	return concatSlice(flowLogsRes, vpcDefaultSGRes, restrictedSSH), nil
}

// checkVPCFlowLogs checks that VPC flow logs are enabled
func (v *VPC) checkVPCFlowLogs() ([]Result, error) {
	var vpcRes []Result
	rule := "VPC flow logs must be enabled"

	for _, region := range v.regions {
		regionSession := session.Must(
			session.NewSession(
				aws.NewConfig().WithRegion(region)))
		regionEC2API := ec2.New(regionSession)

		vpcs, err := regionEC2API.DescribeVpcs(nil)
		if err != nil {
			return nil, err
		}

		for _, vpc := range vpcs.Vpcs {
			flowLogs, err := regionEC2API.DescribeFlowLogs(
				&ec2.DescribeFlowLogsInput{Filter: []*ec2.Filter{
					{
						Name:   aws.String("resource-id"),
						Values: []*string{vpc.VpcId},
					},
				}})
			if err != nil {
				return nil, err
			}

			if len(flowLogs.FlowLogs) == 0 {
				vpcRes = append(
					vpcRes,
					v.vpcResult(vpc, rule, false, "VPC flow logs are not enabled"),
				)
			} else {
				vpcRes = append(vpcRes, v.vpcResult(vpc, rule, true, ""))
			}
		}
	}

	return vpcRes, nil
}

// checkVPCDefaultSecurityGroup checks that the default security group has no
// inbound or outbound rules
func (v *VPC) checkVPCDefaultSecurityGroup() ([]Result, error) {
	var vpcRes []Result
	rule := "VPC default security group must have no inbound or outbound rules"

	for _, region := range v.regions {
		regionSession := session.Must(
			session.NewSession(
				aws.NewConfig().WithRegion(region)))
		regionEC2API := ec2.New(regionSession)

		vpcs, err := regionEC2API.DescribeVpcs(nil)
		if err != nil {
			return nil, err
		}

		for _, vpc := range vpcs.Vpcs {
			sgs, err := regionEC2API.DescribeSecurityGroups(
				&ec2.DescribeSecurityGroupsInput{Filters: []*ec2.Filter{
					{
						Name:   aws.String("group-name"),
						Values: []*string{aws.String("default")},
					},
					{
						Name:   aws.String("vpc-id"),
						Values: []*string{vpc.VpcId},
					},
				}})
			if err != nil {
				return nil, err
			}

			for _, sg := range sgs.SecurityGroups {
				if len(sg.IpPermissions) == 0 && len(sg.IpPermissionsEgress) == 0 {
					vpcRes = append(
						vpcRes,
						v.sgResult(sg, rule, true, ""),
					)
				} else {
					vpcRes = append(
						vpcRes,
						v.sgResult(sg, rule, false, "Default security group has inbound or outbound rules"),
					)
				}
			}
		}
	}
	return vpcRes, nil
}

// checkRestrictedSSH checks that SSH is restricted, i.e. not accessible from
// 0.0.0.0/0 or ::/0
func (v *VPC) checkRestrictedSSH() ([]Result, error) {
	var vpcRes []Result
	rule := "SSH must not be accessible from 0.0.0.0/0 or ::/0"

	for _, region := range v.regions {
		regionSession := session.Must(
			session.NewSession(
				aws.NewConfig().WithRegion(region)))
		regionEC2API := ec2.New(regionSession)

		vpcs, err := regionEC2API.DescribeVpcs(nil)
		if err != nil {
			return nil, err
		}

		for _, vpc := range vpcs.Vpcs {
			sgs, err := regionEC2API.DescribeSecurityGroups(
				&ec2.DescribeSecurityGroupsInput{Filters: []*ec2.Filter{
					{
						Name:   aws.String("vpc-id"),
						Values: []*string{vpc.VpcId},
					},
				}})
			if err != nil {
				return nil, err
			}

		NEXTSG:
			for _, sg := range sgs.SecurityGroups {
				for _, ipPermission := range sg.IpPermissions {
					for _, ipRange := range ipPermission.IpRanges {
						if aws.StringValue(ipRange.CidrIp) == "0.0.0.0/0" &&
							aws.Int64Value(ipPermission.FromPort) <= 22 &&
							aws.Int64Value(ipPermission.ToPort) >= 22 &&
							aws.StringValue(ipPermission.IpProtocol) == "tcp" {
							vpcRes = append(
								vpcRes,
								v.sgResult(sg, rule, false, "SSH is accessible from all IPv4 Addresses"),
							)
							continue NEXTSG
						}
					}

					for _, ipRange := range ipPermission.Ipv6Ranges {
						if aws.StringValue(ipRange.CidrIpv6) == "::/0" &&
							aws.Int64Value(ipPermission.FromPort) <= 22 &&
							aws.Int64Value(ipPermission.ToPort) >= 22 &&
							aws.StringValue(ipPermission.IpProtocol) == "tcp" {
							vpcRes = append(
								vpcRes,
								v.sgResult(sg, rule, false, "SSH is accessible from all IPv6 Addresses"),
							)
							continue NEXTSG
						}
					}
				}

				vpcRes = append(
					vpcRes,
					v.sgResult(sg, rule, true, ""),
				)
			}
		}
	}
	return vpcRes, nil
}

func (v *VPC) vpcResult(vpc *ec2.Vpc, rule string, compliant bool, reason string) Result {
	return Result{
		Resource: Resource{
			Type: "aws/vpc",
			Name: aws.StringValue(vpc.VpcId),
		},
		Rule:      rule,
		Compliant: compliant,
		Reason:    reason,
	}
}

func (v *VPC) sgResult(sg *ec2.SecurityGroup, rule string, compliant bool, reason string) Result {
	return Result{
		Resource: Resource{
			Type: "aws/security-group",
			Name: aws.StringValue(sg.GroupId),
		},
		Rule:      rule,
		Compliant: compliant,
		Reason:    reason,
	}
}

// CloudTrail checks that the user's CloudTrail is SOC2 compliant
type CloudTrail struct {
	cloudTrailAPI *cloudtrail.CloudTrail
	regions       []string
}

// NewCloudTrail returns a new CloudTrail integration
func NewCloudTrail(s *session.Session, regions []string) *CloudTrail {
	return &CloudTrail{cloudTrailAPI: cloudtrail.New(s), regions: regions}
}

// Check checks that the user's CloudTrail is SOC2 compliant
func (c *CloudTrail) Check() ([]Result, error) {
	cloudTrailEncryptionRes, err := c.checkCloudTrailEncryption()
	if err != nil {
		return nil, err
	}

	multiRegionRes, err := c.checkMultiRegionTrail()
	if err != nil {
		return nil, err
	}

	logValidationRes, err := c.checkLogValidation()
	if err != nil {
		return nil, err
	}

	return concatSlice(cloudTrailEncryptionRes, multiRegionRes, logValidationRes), nil
}

// checkCloudTrailEncryption checks that CloudTrail is encrypted
func (c *CloudTrail) checkCloudTrailEncryption() ([]Result, error) {
	var ctRes []Result
	rule := "CloudTrail must be encrypted"

	trails, err := c.cloudTrailAPI.DescribeTrails(nil)
	if err != nil {
		return nil, err
	}

	// first, check for multi-region trails
	for _, trail := range trails.TrailList {
		if !aws.BoolValue(trail.IsMultiRegionTrail) {
			continue
		}

		if aws.StringValue(trail.KmsKeyId) == "" {
			ctRes = append(
				ctRes,
				c.trailResult(trail, rule, false, "CloudTrail is not encrypted"),
			)
			continue
		}
		ctRes = append(ctRes, c.trailResult(trail, rule, true, ""))
	}

	// next, check for single-region trails
	for _, region := range c.regions {
		regionSession := session.Must(
			session.NewSession(
				aws.NewConfig().WithRegion(region)))
		regionCloudTrailAPI := cloudtrail.New(regionSession)

		trails, err := regionCloudTrailAPI.DescribeTrails(nil)
		if err != nil {
			return nil, err
		}

		for _, trail := range trails.TrailList {
			if aws.BoolValue(trail.IsMultiRegionTrail) {
				continue
			}

			if aws.StringValue(trail.KmsKeyId) == "" {
				ctRes = append(
					ctRes,
					c.trailResult(trail, rule, false, "CloudTrail is not encrypted"),
				)
				continue
			}
			ctRes = append(ctRes, c.trailResult(trail, rule, true, ""))
		}
	}

	return ctRes, nil
}

// checkMultiRegionTrail checks that CloudTrail has at least one multi-region
// trail enabled
func (c *CloudTrail) checkMultiRegionTrail() ([]Result, error) {
	rule := "CloudTrail must have multi-region trails enabled"

	trails, err := c.cloudTrailAPI.DescribeTrails(nil)
	if err != nil {
		return nil, err
	}

	for _, trail := range trails.TrailList {
		if aws.BoolValue(trail.IsMultiRegionTrail) {
			eventSelectors, err := c.cloudTrailAPI.GetEventSelectors(
				&cloudtrail.GetEventSelectorsInput{
					TrailName: trail.Name,
				})
			if err != nil {
				return nil, err
			}
			if len(eventSelectors.EventSelectors) != 0 {
				for _, selector := range eventSelectors.EventSelectors {
					// Any event selector matching an event is logged, so this
					// trail meets the rule requirements.
					if aws.BoolValue(selector.IncludeManagementEvents) && len(selector.ExcludeManagementEventSources) == 0 {
						return []Result{c.trailResult(trail, rule, true, "")}, nil
					}
				}
			}
			// TODO: determine if trails with advanced event selectors can log
			// all required events
		}
	}

	return []Result{{
		Resource: Resource{
			Type: "aws/cloudtrail",
			Name: "N/A",
		},
		Rule:      rule,
		Compliant: false,
		Reason:    "CloudTrail does not have multi-region trails enabled",
	}}, nil
}

// checkLogValidation checks that CloudTrail log file validation is enabled
func (c *CloudTrail) checkLogValidation() ([]Result, error) {
	var ctRes []Result
	rule := "CloudTrail must have log file validation enabled"

	trails, err := c.cloudTrailAPI.DescribeTrails(nil)
	if err != nil {
		return nil, err
	}

	for _, trail := range trails.TrailList {
		if aws.BoolValue(trail.LogFileValidationEnabled) {
			ctRes = append(ctRes, c.trailResult(trail, rule, true, ""))
			continue
		}
		ctRes = append(
			ctRes,
			c.trailResult(trail, rule, false, "CloudTrail does not have log file validation enabled"),
		)
	}

	return ctRes, nil
}

func (c *CloudTrail) trailResult(trail *cloudtrail.Trail, rule string, compliant bool, reason string) Result {
	return Result{
		Resource: Resource{
			Type: "aws/cloudtrail",
			Name: aws.StringValue(trail.Name),
		},
		Rule:      rule,
		Compliant: compliant,
		Reason:    reason,
	}
}
