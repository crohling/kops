package kutil

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"fmt"
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/service/autoscaling"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/elb"
	"github.com/golang/glog"
	"io"
	"k8s.io/kops/upup/pkg/fi"
	"k8s.io/kops/upup/pkg/fi/cloudup/awsup"
	"strings"
	"sync"
	"time"
)

// DeleteCluster implements deletion of cluster cloud resources
// The algorithm is pretty simple: it discovers all the resources it can (primary using tags),
// and then it repeatedly attempts to delete them all until they are all deleted.
// There are a few tweaks to that approach, like choosing a default ordering, but it is not much
// smarter.  Cluster deletion is a fairly rare operation anyway, and also some dependencies are invisible
// (e.g. ELB dependencies).
type DeleteCluster struct {
	ClusterName string
	Region      string
	Cloud       fi.Cloud
}

type ResourceTracker struct {
	Name string
	Type string
	ID   string

	blocks  []string
	blocked []string
	done    bool

	deleter func(cloud fi.Cloud, tracker *ResourceTracker) error
}

type listFn func(fi.Cloud, string) ([]*ResourceTracker, error)

func gunzipBytes(d []byte) ([]byte, error) {
	var out bytes.Buffer
	in := bytes.NewReader(d)
	r, err := gzip.NewReader(in)
	if err != nil {
		return nil, fmt.Errorf("error building gunzip reader: %v", err)
	}
	defer r.Close()
	_, err = io.Copy(&out, r)
	if err != nil {
		return nil, fmt.Errorf("error decompressing data: %v", err)
	}
	return out.Bytes(), nil
}

func buildEC2Filters(cloud fi.Cloud) []*ec2.Filter {
	awsCloud := cloud.(*awsup.AWSCloud)
	tags := awsCloud.Tags()

	var filters []*ec2.Filter
	for k, v := range tags {
		filter := awsup.NewEC2Filter("tag:"+k, v)
		filters = append(filters, filter)
	}
	return filters
}

func (c *DeleteCluster) ListResources() (map[string]*ResourceTracker, error) {
	cloud := c.Cloud.(*awsup.AWSCloud)

	resources := make(map[string]*ResourceTracker)

	listFunctions := []listFn{
		ListSubnets, ListRouteTables, ListSecurityGroups,
		ListInstances, ListDhcpOptions, ListInternetGateways, ListVPCs, ListVolumes,
		// ELBs
		ListELBs,
		// ASG
		ListAutoScalingGroups,
		ListAutoScalingLaunchConfigurations,
	}
	for _, fn := range listFunctions {
		trackers, err := fn(cloud, c.ClusterName)
		if err != nil {
			return nil, err
		}
		for _, t := range trackers {
			resources[t.Type+":"+t.ID] = t
		}
	}

	{
		// Gateways weren't tagged in kube-up
		// If we are deleting the VPC, we should delete the attached gateway
		// (no real reason not to; easy to recreate; no real state etc)

		gateways, err := DescribeInternetGatewaysIgnoreTags(cloud)
		if err != nil {
			return nil, err
		}

		for _, igw := range gateways {
			for _, attachment := range igw.Attachments {
				vpcID := aws.StringValue(attachment.VpcId)
				igwID := aws.StringValue(igw.InternetGatewayId)
				if vpcID == "" || igwID == "" {
					continue
				}
				if resources["vpc:"+vpcID] != nil && resources["internet-gateway:"+igwID] == nil {
					resources["internet-gateway:"+igwID] = &ResourceTracker{
						Name:    FindName(igw.Tags),
						ID:      igwID,
						Type:    "internet-gateway",
						deleter: DeleteInternetGateway,
					}
				}
			}
		}
	}

	for k, t := range resources {
		if t.done {
			delete(resources, k)
		}
	}
	return resources, nil
}

func (c *DeleteCluster) DeleteResources(resources map[string]*ResourceTracker) error {
	depMap := make(map[string][]string)

	done := make(map[string]*ResourceTracker)

	var mutex sync.Mutex

	for k, t := range resources {
		for _, block := range t.blocks {
			depMap[block] = append(depMap[block], k)
		}

		for _, blocked := range t.blocked {
			depMap[k] = append(depMap[k], blocked)
		}

		if t.done {
			done[k] = t
		}
	}

	glog.V(2).Infof("Dependencies")
	for k, v := range depMap {
		glog.V(2).Infof("\t%s\t%v", k, v)
	}

	iterationsWithNoProgress := 0
	for {
		// TODO: Some form of default ordering based on types?
		// TODO: Give up eventually?

		failed := make(map[string]*ResourceTracker)

		for {
			phase := make(map[string]*ResourceTracker)

			for k, r := range resources {
				if _, d := done[k]; d {
					continue
				}

				if _, d := failed[k]; d {
					// Only attempt each resource once per pass
					continue
				}

				ready := true
				for _, dep := range depMap[k] {
					if _, d := done[dep]; !d {
						glog.V(4).Infof("dependency %q of %q not deleted; skipping", dep, k)
						ready = false
					}
				}
				if !ready {
					continue
				}

				phase[k] = r
			}

			if len(phase) == 0 {
				break
			}

			var wg sync.WaitGroup
			for k, t := range phase {
				wg.Add(1)

				go func(k string, t *ResourceTracker) {
					mutex.Lock()
					failed[k] = t
					mutex.Unlock()

					defer wg.Done()
					glog.V(4).Infof("Deleting resource %s:  ", k)

					err := t.deleter(c.Cloud, t)
					if err != nil {
						mutex.Lock()
						if IsDependencyViolation(err) {
							fmt.Printf("%s\tstill has dependencies, will retry\n", k)
							glog.V(4).Infof("API call made when had dependency %s", k)
						} else {
							fmt.Printf("%s\terror deleting resource, will retry: %v\n", k, err)
						}
						failed[k] = t
						mutex.Unlock()
					} else {
						mutex.Lock()
						fmt.Printf("%s\tok\n", k)

						delete(failed, k)
						iterationsWithNoProgress = 0
						done[k] = t
						mutex.Unlock()
					}
				}(k, t)
			}
			wg.Wait()
		}

		if len(resources) == len(done) {
			return nil
		}

		fmt.Printf("Not all resources deleted; waiting before reattempting deletion\n")
		for k := range resources {
			if _, d := done[k]; d {
				continue
			}

			fmt.Printf("\t%s\n", k)
		}

		iterationsWithNoProgress++
		if iterationsWithNoProgress > 30 {
			return fmt.Errorf("Not making progress deleting resources; giving up")
		}

		time.Sleep(10 * time.Second)
	}
}

func matchesAsgTags(tags map[string]string, actual []*autoscaling.TagDescription) bool {
	for k, v := range tags {
		found := false
		for _, a := range actual {
			if aws.StringValue(a.Key) == k {
				if aws.StringValue(a.Value) == v {
					found = true
					break
				}
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func matchesElbTags(tags map[string]string, actual []*elb.Tag) bool {
	for k, v := range tags {
		found := false
		for _, a := range actual {
			if aws.StringValue(a.Key) == k {
				if aws.StringValue(a.Value) == v {
					found = true
					break
				}
			}
		}
		if !found {
			return false
		}
	}
	return true
}

//type DeletableResource interface {
//	Delete(cloud fi.Cloud) error
//}

func DeleteInstance(cloud fi.Cloud, t *ResourceTracker) error {
	c := cloud.(*awsup.AWSCloud)

	id := t.ID
	glog.V(2).Infof("Deleting EC2 instance %q", id)
	request := &ec2.TerminateInstancesInput{
		InstanceIds: []*string{&id},
	}
	_, err := c.EC2.TerminateInstances(request)
	if err != nil {
		return fmt.Errorf("error deleting instance %q: %v", id, err)
	}
	return nil
}

func ListInstances(cloud fi.Cloud, clusterName string) ([]*ResourceTracker, error) {
	c := cloud.(*awsup.AWSCloud)

	glog.V(2).Infof("Querying EC2 instances")
	request := &ec2.DescribeInstancesInput{
		Filters: buildEC2Filters(cloud),
	}

	var trackers []*ResourceTracker

	err := c.EC2.DescribeInstancesPages(request, func(p *ec2.DescribeInstancesOutput, lastPage bool) bool {
		for _, reservation := range p.Reservations {
			for _, instance := range reservation.Instances {
				id := aws.StringValue(instance.InstanceId)

				if instance.State != nil {
					stateName := aws.StringValue(instance.State.Name)
					switch stateName {
					case "terminated":
						continue

					case "running":
					case "stopped":
						// We need to delete
						glog.V(4).Infof("instance %q has state=%q", id, stateName)

					default:
						glog.Infof("unknown instance state for %q: %q", id, stateName)
					}
				}

				tracker := &ResourceTracker{
					Name:    FindName(instance.Tags),
					ID:      id,
					Type:    "instance",
					deleter: DeleteInstance,
				}

				var blocks []string
				blocks = append(blocks, "vpc:"+aws.StringValue(instance.VpcId))

				for _, volume := range instance.BlockDeviceMappings {
					if volume.Ebs == nil {
						continue
					}
					blocks = append(blocks, "volume:"+aws.StringValue(volume.Ebs.VolumeId))
				}
				for _, sg := range instance.SecurityGroups {
					blocks = append(blocks, "security-group:"+aws.StringValue(sg.GroupId))
				}
				blocks = append(blocks, "subnet:"+aws.StringValue(instance.SubnetId))
				blocks = append(blocks, "vpc:"+aws.StringValue(instance.VpcId))

				tracker.blocks = blocks

				trackers = append(trackers, tracker)

			}
		}
		return true
	})
	if err != nil {
		return nil, fmt.Errorf("error describing instances: %v", err)
	}

	return trackers, nil
}

func DeleteSecurityGroup(cloud fi.Cloud, t *ResourceTracker) error {
	c := cloud.(*awsup.AWSCloud)

	id := t.ID
	// First clear all inter-dependent rules
	// TODO: Move to a "pre-execute" phase?
	{
		request := &ec2.DescribeSecurityGroupsInput{
			GroupIds: []*string{&id},
		}
		response, err := c.EC2.DescribeSecurityGroups(request)
		if err != nil {
			return fmt.Errorf("error describing SecurityGroup %q: %v", id, err)
		}

		if len(response.SecurityGroups) == 0 {
			return nil
		}
		if len(response.SecurityGroups) != 1 {
			return fmt.Errorf("found mutiple SecurityGroups with ID %q", id)
		}
		sg := response.SecurityGroups[0]

		if len(sg.IpPermissions) != 0 {
			revoke := &ec2.RevokeSecurityGroupIngressInput{
				GroupId:       &id,
				IpPermissions: sg.IpPermissions,
			}
			_, err = c.EC2.RevokeSecurityGroupIngress(revoke)
			if err != nil {
				return fmt.Errorf("cannot revoke ingress for ID %q: %v", id, err)
			}
		}
	}

	{
		glog.V(2).Infof("Deleting EC2 SecurityGroup %q", id)
		request := &ec2.DeleteSecurityGroupInput{
			GroupId: &id,
		}
		_, err := c.EC2.DeleteSecurityGroup(request)
		if err != nil {
			if IsDependencyViolation(err) {
				return err
			}
			return fmt.Errorf("error deleting SecurityGroup %q: %v", id, err)
		}
	}
	return nil
}

func ListSecurityGroups(cloud fi.Cloud, clusterName string) ([]*ResourceTracker, error) {
	c := cloud.(*awsup.AWSCloud)

	glog.V(2).Infof("Listing EC2 SecurityGroups")
	request := &ec2.DescribeSecurityGroupsInput{
		Filters: buildEC2Filters(cloud),
	}
	response, err := c.EC2.DescribeSecurityGroups(request)
	if err != nil {
		return nil, fmt.Errorf("error listing SecurityGroups: %v", err)
	}

	var trackers []*ResourceTracker

	for _, sg := range response.SecurityGroups {
		tracker := &ResourceTracker{
			Name:    FindName(sg.Tags),
			ID:      aws.StringValue(sg.GroupId),
			Type:    "security-group",
			deleter: DeleteSecurityGroup,
		}

		var blocks []string
		blocks = append(blocks, "vpc:"+aws.StringValue(sg.VpcId))

		tracker.blocks = blocks

		trackers = append(trackers, tracker)
	}

	return trackers, nil
}

func DeleteVolume(cloud fi.Cloud, r *ResourceTracker) error {
	c := cloud.(*awsup.AWSCloud)

	id := r.ID

	glog.V(2).Infof("Deleting EC2 Volume %q", id)
	request := &ec2.DeleteVolumeInput{
		VolumeId: &id,
	}
	_, err := c.EC2.DeleteVolume(request)
	if err != nil {
		if IsDependencyViolation(err) {
			return err
		}
		if AWSErrorCode(err) == "InvalidVolume.NotFound" {
			// Concurrently deleted
			return nil
		}
		return fmt.Errorf("error deleting Volume %q: %v", id, err)
	}
	return nil
}

func ListVolumes(cloud fi.Cloud, clusterName string) ([]*ResourceTracker, error) {
	c := cloud.(*awsup.AWSCloud)

	volumes, err := DescribeVolumes(cloud)
	if err != nil {
		return nil, err
	}
	var trackers []*ResourceTracker

	elasticIPs := make(map[string]bool)
	for _, volume := range volumes {
		id := aws.StringValue(volume.VolumeId)

		tracker := &ResourceTracker{
			Name:    FindName(volume.Tags),
			ID:      id,
			Type:    "volume",
			deleter: DeleteVolume,
		}

		var blocks []string
		//blocks = append(blocks, "vpc:" + aws.StringValue(rt.VpcId))

		tracker.blocks = blocks

		trackers = append(trackers, tracker)

		// Check for an elastic IP tag
		for _, tag := range volume.Tags {
			name := aws.StringValue(tag.Key)
			ip := ""
			if name == "kubernetes.io/master-ip" {
				ip = aws.StringValue(tag.Value)
			}
			if ip != "" {
				elasticIPs[ip] = true
			}
		}

	}

	if len(elasticIPs) != 0 {
		glog.V(2).Infof("Querying EC2 Elastic IPs")
		request := &ec2.DescribeAddressesInput{}
		response, err := c.EC2.DescribeAddresses(request)
		if err != nil {
			return nil, fmt.Errorf("error describing addresses: %v", err)
		}

		for _, address := range response.Addresses {
			ip := aws.StringValue(address.PublicIp)
			if !elasticIPs[ip] {
				continue
			}

			tracker := &ResourceTracker{
				Name:    ip,
				ID:      aws.StringValue(address.AllocationId),
				Type:    "elastic-ip",
				deleter: DeleteElasticIP,
			}

			trackers = append(trackers, tracker)

		}
	}

	return trackers, nil
}

func DescribeVolumes(cloud fi.Cloud) ([]*ec2.Volume, error) {
	c := cloud.(*awsup.AWSCloud)

	var volumes []*ec2.Volume

	glog.V(2).Infof("Listing EC2 Volumes")
	request := &ec2.DescribeVolumesInput{
		Filters: buildEC2Filters(c),
	}

	err := c.EC2.DescribeVolumesPages(request, func(p *ec2.DescribeVolumesOutput, lastPage bool) bool {
		for _, volume := range p.Volumes {
			volumes = append(volumes, volume)
		}
		return true
	})
	if err != nil {
		return nil, fmt.Errorf("error describing volumes: %v", err)
	}

	return volumes, nil
}

// AWSErrorCode returns the aws error code, if it is an awserr.Error, otherwise ""
func AWSErrorCode(err error) string {
	if awsError, ok := err.(awserr.Error); ok {
		return awsError.Code()
	}
	return ""
}

func IsDependencyViolation(err error) bool {
	code := AWSErrorCode(err)
	switch code {
	case "":
		return false
	case "DependencyViolation", "VolumeInUse", "InvalidIPAddress.InUse":
		return true
	default:
		glog.Infof("unexpected aws error code: %q", code)
		return false
	}
}

func DeleteSubnet(cloud fi.Cloud, tracker *ResourceTracker) error {
	c := cloud.(*awsup.AWSCloud)

	id := tracker.ID

	glog.V(2).Infof("Deleting EC2 Subnet %q", id)
	request := &ec2.DeleteSubnetInput{
		SubnetId: &id,
	}
	_, err := c.EC2.DeleteSubnet(request)
	if err != nil {
		if IsDependencyViolation(err) {
			return err
		}
		return fmt.Errorf("error deleting Subnet %q: %v", id, err)
	}
	return nil
}

func ListSubnets(cloud fi.Cloud, clusterName string) ([]*ResourceTracker, error) {
	subnets, err := DescribeSubnets(cloud)
	if err != nil {
		return nil, fmt.Errorf("error listing subnets: %v", err)
	}

	var trackers []*ResourceTracker

	for _, subnet := range subnets {
		tracker := &ResourceTracker{
			Name:    FindName(subnet.Tags),
			ID:      aws.StringValue(subnet.SubnetId),
			Type:    "subnet",
			deleter: DeleteSubnet,
		}

		var blocks []string
		blocks = append(blocks, "vpc:"+aws.StringValue(subnet.VpcId))

		tracker.blocks = blocks

		trackers = append(trackers, tracker)
	}

	return trackers, nil
}

func DescribeSubnets(cloud fi.Cloud) ([]*ec2.Subnet, error) {
	c := cloud.(*awsup.AWSCloud)

	glog.V(2).Infof("Listing EC2 subnets")
	request := &ec2.DescribeSubnetsInput{
		Filters: buildEC2Filters(cloud),
	}
	response, err := c.EC2.DescribeSubnets(request)
	if err != nil {
		return nil, fmt.Errorf("error listing subnets: %v", err)
	}

	return response.Subnets, nil
}

func DeleteRouteTable(cloud fi.Cloud, r *ResourceTracker) error {
	c := cloud.(*awsup.AWSCloud)

	id := r.ID

	glog.V(2).Infof("Deleting EC2 RouteTable %q", id)
	request := &ec2.DeleteRouteTableInput{
		RouteTableId: &id,
	}
	_, err := c.EC2.DeleteRouteTable(request)
	if err != nil {
		if IsDependencyViolation(err) {
			return err
		}
		return fmt.Errorf("error deleting RouteTable %q: %v", id, err)
	}
	return nil
}

func ListRouteTables(cloud fi.Cloud, clusterName string) ([]*ResourceTracker, error) {
	c := cloud.(*awsup.AWSCloud)

	glog.V(2).Infof("Listing EC2 RouteTables")
	request := &ec2.DescribeRouteTablesInput{
		Filters: buildEC2Filters(cloud),
	}
	response, err := c.EC2.DescribeRouteTables(request)
	if err != nil {
		return nil, fmt.Errorf("error listing RouteTables: %v", err)
	}

	var trackers []*ResourceTracker

	for _, rt := range response.RouteTables {
		tracker := &ResourceTracker{
			Name:    FindName(rt.Tags),
			ID:      aws.StringValue(rt.RouteTableId),
			Type:    "route-table",
			deleter: DeleteRouteTable,
		}

		var blocks []string
		var blocked []string

		blocks = append(blocks, "vpc:"+aws.StringValue(rt.VpcId))

		for _, a := range rt.Associations {
			blocked = append(blocked, "subnet:"+aws.StringValue(a.SubnetId))
		}

		tracker.blocks = blocks
		tracker.blocked = blocked

		trackers = append(trackers, tracker)
	}

	return trackers, nil
}

func DeleteDhcpOptions(cloud fi.Cloud, r *ResourceTracker) error {
	c := cloud.(*awsup.AWSCloud)

	id := r.ID

	glog.V(2).Infof("Deleting EC2 DhcpOptions %q", id)
	request := &ec2.DeleteDhcpOptionsInput{
		DhcpOptionsId: &id,
	}
	_, err := c.EC2.DeleteDhcpOptions(request)
	if err != nil {
		if IsDependencyViolation(err) {
			return err
		}
		return fmt.Errorf("error deleting DhcpOptions %q: %v", id, err)
	}
	return nil
}

func ListDhcpOptions(cloud fi.Cloud, clusterName string) ([]*ResourceTracker, error) {
	dhcpOptions, err := DescribeDhcpOptions(cloud)
	if err != nil {
		return nil, err
	}

	var trackers []*ResourceTracker

	for _, o := range dhcpOptions {
		tracker := &ResourceTracker{
			Name:    FindName(o.Tags),
			ID:      aws.StringValue(o.DhcpOptionsId),
			Type:    "dhcp-options",
			deleter: DeleteDhcpOptions,
		}

		var blocks []string

		tracker.blocks = blocks

		trackers = append(trackers, tracker)
	}

	return trackers, nil
}

func DescribeDhcpOptions(cloud fi.Cloud) ([]*ec2.DhcpOptions, error) {
	c := cloud.(*awsup.AWSCloud)

	glog.V(2).Infof("Listing EC2 DhcpOptions")
	request := &ec2.DescribeDhcpOptionsInput{
		Filters: buildEC2Filters(cloud),
	}
	response, err := c.EC2.DescribeDhcpOptions(request)
	if err != nil {
		return nil, fmt.Errorf("error listing DhcpOptions: %v", err)
	}

	return response.DhcpOptions, nil
}

func DeleteInternetGateway(cloud fi.Cloud, r *ResourceTracker) error {
	c := cloud.(*awsup.AWSCloud)

	id := r.ID

	var igw *ec2.InternetGateway
	{
		request := &ec2.DescribeInternetGatewaysInput{
			InternetGatewayIds: []*string{&id},
		}
		response, err := c.EC2.DescribeInternetGateways(request)
		if err != nil {
			if AWSErrorCode(err) == "InvalidInternetGatewayID.NotFound" {
				glog.Infof("Internet gateway %q not found; assuming already deleted", id)
				return nil
			}

			return fmt.Errorf("error describing InternetGateway %q: %v", id, err)
		}
		if response == nil || len(response.InternetGateways) == 0 {
			return nil
		}
		if len(response.InternetGateways) != 1 {
			return fmt.Errorf("found multiple InternetGateways with id %q", id)
		}
		igw = response.InternetGateways[0]
	}

	for _, a := range igw.Attachments {
		glog.V(2).Infof("Detaching EC2 InternetGateway %q", id)
		request := &ec2.DetachInternetGatewayInput{
			InternetGatewayId: &id,
			VpcId:             a.VpcId,
		}
		_, err := c.EC2.DetachInternetGateway(request)
		if err != nil {
			if IsDependencyViolation(err) {
				return err
			}
			return fmt.Errorf("error detaching InternetGateway %q: %v", id, err)
		}
	}

	{
		glog.V(2).Infof("Deleting EC2 InternetGateway %q", id)
		request := &ec2.DeleteInternetGatewayInput{
			InternetGatewayId: &id,
		}
		_, err := c.EC2.DeleteInternetGateway(request)
		if err != nil {
			if IsDependencyViolation(err) {
				return err
			}
			if AWSErrorCode(err) == "InvalidInternetGatewayID.NotFound" {
				glog.Infof("Internet gateway %q not found; assuming already deleted", id)
				return nil
			}
			return fmt.Errorf("error deleting InternetGateway %q: %v", id, err)
		}
	}

	return nil
}

func ListInternetGateways(cloud fi.Cloud, clusterName string) ([]*ResourceTracker, error) {
	gateways, err := DescribeInternetGateways(cloud)
	if err != nil {
		return nil, err
	}

	var trackers []*ResourceTracker

	for _, o := range gateways {
		tracker := &ResourceTracker{
			Name:    FindName(o.Tags),
			ID:      aws.StringValue(o.InternetGatewayId),
			Type:    "internet-gateway",
			deleter: DeleteInternetGateway,
		}

		var blocks []string
		for _, a := range o.Attachments {
			if aws.StringValue(a.VpcId) != "" {
				blocks = append(blocks, "vpc:"+aws.StringValue(a.VpcId))
			}
		}
		tracker.blocks = blocks

		trackers = append(trackers, tracker)
	}

	return trackers, nil
}

func DescribeInternetGateways(cloud fi.Cloud) ([]*ec2.InternetGateway, error) {
	c := cloud.(*awsup.AWSCloud)

	glog.V(2).Infof("Listing EC2 InternetGateways")
	request := &ec2.DescribeInternetGatewaysInput{
		Filters: buildEC2Filters(cloud),
	}
	response, err := c.EC2.DescribeInternetGateways(request)
	if err != nil {
		return nil, fmt.Errorf("error listing InternetGateway: %v", err)
	}

	var gateways []*ec2.InternetGateway
	for _, o := range response.InternetGateways {
		gateways = append(gateways, o)
	}

	return gateways, nil
}

// DescribeInternetGatewaysIgnoreTags returns all ec2.InternetGateways, ignoring tags
// (gateways were not always tagged in kube-up)
func DescribeInternetGatewaysIgnoreTags(cloud fi.Cloud) ([]*ec2.InternetGateway, error) {
	c := cloud.(*awsup.AWSCloud)

	glog.V(2).Infof("Listing all Internet Gateways")

	request := &ec2.DescribeInternetGatewaysInput{}
	response, err := c.EC2.DescribeInternetGateways(request)
	if err != nil {
		return nil, fmt.Errorf("error listing (all) InternetGateways: %v", err)
	}

	var gateways []*ec2.InternetGateway

	for _, igw := range response.InternetGateways {
		gateways = append(gateways, igw)
	}

	return gateways, nil
}

func DeleteVPC(cloud fi.Cloud, r *ResourceTracker) error {
	c := cloud.(*awsup.AWSCloud)

	id := r.ID

	glog.V(2).Infof("Deleting EC2 VPC %q", id)
	request := &ec2.DeleteVpcInput{
		VpcId: &id,
	}
	_, err := c.EC2.DeleteVpc(request)
	if err != nil {
		if IsDependencyViolation(err) {
			return err
		}
		return fmt.Errorf("error deleting VPC %q: %v", id, err)
	}
	return nil
}

func ListVPCs(cloud fi.Cloud, clusterName string) ([]*ResourceTracker, error) {
	c := cloud.(*awsup.AWSCloud)

	glog.V(2).Infof("Listing EC2 VPC")
	request := &ec2.DescribeVpcsInput{
		Filters: buildEC2Filters(cloud),
	}
	response, err := c.EC2.DescribeVpcs(request)
	if err != nil {
		return nil, fmt.Errorf("error listing VPCs: %v", err)
	}

	var trackers []*ResourceTracker

	for _, v := range response.Vpcs {
		tracker := &ResourceTracker{
			Name:    FindName(v.Tags),
			ID:      aws.StringValue(v.VpcId),
			Type:    "vpc",
			deleter: DeleteVPC,
		}

		var blocks []string
		blocks = append(blocks, "dhcp-options:"+aws.StringValue(v.DhcpOptionsId))

		tracker.blocks = blocks

		trackers = append(trackers, tracker)
	}

	return trackers, nil
}

func DeleteAutoScalingGroup(cloud fi.Cloud, r *ResourceTracker) error {
	c := cloud.(*awsup.AWSCloud)

	id := r.ID

	glog.V(2).Infof("Deleting autoscaling group %q", id)
	request := &autoscaling.DeleteAutoScalingGroupInput{
		AutoScalingGroupName: &id,
		ForceDelete:          aws.Bool(true),
	}
	_, err := c.Autoscaling.DeleteAutoScalingGroup(request)
	if err != nil {
		if IsDependencyViolation(err) {
			return err
		}
		return fmt.Errorf("error deleting autoscaling group %q: %v", id, err)
	}
	return nil
}

func ListAutoScalingGroups(cloud fi.Cloud, clusterName string) ([]*ResourceTracker, error) {
	c := cloud.(*awsup.AWSCloud)

	tags := c.Tags()

	asgs, err := findAutoscalingGroups(c, tags)
	if err != nil {
		return nil, err
	}

	var trackers []*ResourceTracker

	for _, asg := range asgs {
		tracker := &ResourceTracker{
			Name:    FindASGName(asg.Tags),
			ID:      aws.StringValue(asg.AutoScalingGroupName),
			Type:    "autoscaling-group",
			deleter: DeleteAutoScalingGroup,
		}

		var blocks []string
		subnets := aws.StringValue(asg.VPCZoneIdentifier)
		for _, subnet := range strings.Split(subnets, ",") {
			if subnet == "" {
				continue
			}
			blocks = append(blocks, "subnet:"+subnet)
		}
		blocks = append(blocks, "launchconfig:"+aws.StringValue(asg.LaunchConfigurationName))

		tracker.blocks = blocks

		trackers = append(trackers, tracker)
	}

	return trackers, nil
}

func ListAutoScalingLaunchConfigurations(cloud fi.Cloud, clusterName string) ([]*ResourceTracker, error) {
	c := cloud.(*awsup.AWSCloud)

	glog.V(2).Infof("Listing all Autoscaling LaunchConfigurations for cluster %q", clusterName)

	var trackers []*ResourceTracker

	request := &autoscaling.DescribeLaunchConfigurationsInput{}
	err := c.Autoscaling.DescribeLaunchConfigurationsPages(request, func(p *autoscaling.DescribeLaunchConfigurationsOutput, lastPage bool) bool {
		for _, t := range p.LaunchConfigurations {
			if t.UserData == nil {
				continue
			}

			b, err := base64.StdEncoding.DecodeString(aws.StringValue(t.UserData))
			if err != nil {
				glog.Infof("Ignoring autoscaling LaunchConfiguration with invalid UserData: %v", *t.LaunchConfigurationName)
				continue
			}

			userData, err := UserDataToString(b)
			if err != nil {
				glog.Infof("Ignoring autoscaling LaunchConfiguration with invalid UserData: %v", *t.LaunchConfigurationName)
				continue
			}

			glog.V(8).Infof("UserData: %s", string(userData))

			var matchStrings []string

			// TODO: reintroduce
			//clusterLocationLine := "ClusterLocation: s3://clusters.awsdata.com/upgraded.awsdata.com/cluster.spec\n"
			//isNodeupConfig := strings.Contains(string(userData), clusterLocationLine)

			// V1
			matchStrings = append(matchStrings, "\nINSTANCE_PREFIX: "+clusterName+"\n")
			matchStrings = append(matchStrings, "\nINSTANCE_PREFIX: '"+clusterName+"'\n")

			match := false
			for _, m := range matchStrings {
				if strings.Contains(string(userData), m) {
					match = true
				}
			}
			if match {
				tracker := &ResourceTracker{
					Name:    aws.StringValue(t.LaunchConfigurationName),
					ID:      aws.StringValue(t.LaunchConfigurationName),
					Type:    "launchconfig",
					deleter: DeleteAutoscalingLaunchConfiguration,
				}

				var blocks []string
				//blocks = append(blocks, "launchconfig:" + aws.StringValue(asg.LaunchConfigurationName))

				tracker.blocks = blocks

				trackers = append(trackers, tracker)
			}
		}
		return true
	})
	if err != nil {
		return nil, fmt.Errorf("error listing autoscaling LaunchConfigurations: %v", err)
	}

	return trackers, nil
}

func DeleteAutoscalingLaunchConfiguration(cloud fi.Cloud, r *ResourceTracker) error {
	c := cloud.(*awsup.AWSCloud)

	id := r.ID
	glog.V(2).Infof("Deleting autoscaling LaunchConfiguration %q", id)
	request := &autoscaling.DeleteLaunchConfigurationInput{
		LaunchConfigurationName: &id,
	}
	_, err := c.Autoscaling.DeleteLaunchConfiguration(request)
	if err != nil {
		return fmt.Errorf("error deleting autoscaling LaunchConfiguration %q: %v", id, err)
	}
	return nil
}

func DeleteELB(cloud fi.Cloud, r *ResourceTracker) error {
	c := cloud.(*awsup.AWSCloud)

	id := r.ID

	glog.V(2).Infof("Deleting ELB %q", id)
	request := &elb.DeleteLoadBalancerInput{
		LoadBalancerName: &id,
	}
	_, err := c.ELB.DeleteLoadBalancer(request)
	if err != nil {
		if IsDependencyViolation(err) {
			return err
		}
		return fmt.Errorf("error deleting LoadBalancer %q: %v", id, err)
	}
	return nil
}

func ListELBs(cloud fi.Cloud, clusterName string) ([]*ResourceTracker, error) {
	elbs, elbTags, err := DescribeELBs(cloud)
	if err != nil {
		return nil, err
	}

	var trackers []*ResourceTracker
	for _, elb := range elbs {
		id := aws.StringValue(elb.LoadBalancerName)
		tracker := &ResourceTracker{
			Name:    FindELBName(elbTags[id]),
			ID:      id,
			Type:    "load-balancer",
			deleter: DeleteELB,
		}

		var blocks []string
		for _, sg := range elb.SecurityGroups {
			blocks = append(blocks, "security-group:"+aws.StringValue(sg))
		}
		for _, s := range elb.Subnets {
			blocks = append(blocks, "subnet:"+aws.StringValue(s))
		}
		blocks = append(blocks, "vpc:"+aws.StringValue(elb.VPCId))

		tracker.blocks = blocks

		trackers = append(trackers, tracker)
	}

	return trackers, nil
}

func DescribeELBs(cloud fi.Cloud) ([]*elb.LoadBalancerDescription, map[string][]*elb.Tag, error) {
	c := cloud.(*awsup.AWSCloud)
	tags := c.Tags()

	glog.V(2).Infof("Listing all ELBs")

	request := &elb.DescribeLoadBalancersInput{}

	var elbs []*elb.LoadBalancerDescription
	elbTags := make(map[string][]*elb.Tag)

	var innerError error
	err := c.ELB.DescribeLoadBalancersPages(request, func(p *elb.DescribeLoadBalancersOutput, lastPage bool) bool {
		if len(p.LoadBalancerDescriptions) == 0 {
			return true
		}

		tagRequest := &elb.DescribeTagsInput{}

		nameToELB := make(map[string]*elb.LoadBalancerDescription)
		for _, elb := range p.LoadBalancerDescriptions {
			name := aws.StringValue(elb.LoadBalancerName)
			nameToELB[name] = elb

			tagRequest.LoadBalancerNames = append(tagRequest.LoadBalancerNames, elb.LoadBalancerName)
		}

		tagResponse, err := c.ELB.DescribeTags(tagRequest)
		if err != nil {
			innerError = fmt.Errorf("error listing elb Tags: %v", err)
			return false
		}

		for _, t := range tagResponse.TagDescriptions {
			elbName := aws.StringValue(t.LoadBalancerName)

			if !matchesElbTags(tags, t.Tags) {
				continue
			}

			elbTags[elbName] = t.Tags

			elb := nameToELB[elbName]
			elbs = append(elbs, elb)
		}
		return true
	})
	if err != nil {
		return nil, nil, fmt.Errorf("error describing LoadBalancers: %v", err)
	}
	if innerError != nil {
		return nil, nil, fmt.Errorf("error describing LoadBalancers: %v", innerError)
	}

	return elbs, elbTags, nil
}

func DeleteElasticIP(cloud fi.Cloud, t *ResourceTracker) error {
	c := cloud.(*awsup.AWSCloud)

	id := t.ID

	glog.V(2).Infof("Releasing IP %s", t.Name)
	request := &ec2.ReleaseAddressInput{
		AllocationId: &id,
	}
	_, err := c.EC2.ReleaseAddress(request)
	if err != nil {
		if IsDependencyViolation(err) {
			return err
		}
		return fmt.Errorf("error deleting elastic ip %q: %v", t.Name, err)
	}
	return nil
}

func FindName(tags []*ec2.Tag) string {
	if name, found := awsup.FindEC2Tag(tags, "Name"); found {
		return name
	}
	return ""
}

func FindASGName(tags []*autoscaling.TagDescription) string {
	if name, found := awsup.FindASGTag(tags, "Name"); found {
		return name
	}
	return ""
}

func FindELBName(tags []*elb.Tag) string {
	if name, found := awsup.FindELBTag(tags, "Name"); found {
		return name
	}
	return ""
}
