package remote

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"fmt"
	"time"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc"
	"google.golang.org/grpc/status"

	"github.com/petruekhin/vibe-firecracker/vmlib"
	pb "github.com/petruekhin/vibe-firecracker/vmlib/gen/vmlib/facade/v1"
)

// ---- conversions -------------------------------------------------------------

func vmToPB(v vmlib.VM) *pb.VM {
	return &pb.VM{VmId: v.ID, ImageId: v.ImageID, Profile: v.Profile, State: string(v.State), Labels: v.Labels,
		Retained: v.Retained, RetainedReason: v.RetainedReason}
}

func vmFromPB(v *pb.VM) vmlib.VM {
	return vmlib.VM{ID: v.GetVmId(), ImageID: v.GetImageId(), Profile: v.GetProfile(), State: vmlib.VMState(v.GetState()),
		Labels: v.GetLabels(), Retained: v.GetRetained(), RetainedReason: v.GetRetainedReason()}
}

func facadeImageToPB(i vmlib.Image) *pb.Image {
	return &pb.Image{Id: i.ID, SizeBytes: i.SizeBytes, Sha256: i.SHA256, Base: i.Base}
}

func facadeImageFromPB(i *pb.Image) vmlib.Image {
	return vmlib.Image{ID: i.GetId(), SizeBytes: i.GetSizeBytes(), SHA256: i.GetSha256(), Base: i.GetBase()}
}

func sshToPB(s vmlib.SSHInfo) *pb.SSHInfo {
	out := &pb.SSHInfo{Host: s.Host, Port: uint32(s.Port), User: s.User, AuthMode: s.AuthMode, KeyReference: s.KeyReference,
		PublicKeyProvisioned: s.PublicKeyProvisioned, HostKeyPolicy: s.HostKeyPolicy, HostKeyFingerprint: s.HostKeyFingerprint, Ready: s.Ready,
		Guest: facadeGuestToPB(s.Guest)}
	if s.Bastion != nil {
		out.Bastion = &pb.Bastion{Host: s.Bastion.Host, Port: uint32(s.Bastion.Port), User: s.Bastion.User}
	}
	return out
}

func sshFromPB(s *pb.SSHInfo) vmlib.SSHInfo {
	out := vmlib.SSHInfo{Host: s.GetHost(), Port: int(s.GetPort()), User: s.GetUser(), AuthMode: s.GetAuthMode(), KeyReference: s.GetKeyReference(),
		PublicKeyProvisioned: s.GetPublicKeyProvisioned(), HostKeyPolicy: s.GetHostKeyPolicy(), HostKeyFingerprint: s.GetHostKeyFingerprint(), Ready: s.GetReady(),
		Guest: facadeGuestFromPB(s.GetGuest())}
	if b := s.GetBastion(); b != nil {
		out.Bastion = &vmlib.Bastion{Host: b.GetHost(), Port: int(b.GetPort()), User: b.GetUser()}
	}
	return out
}

func facadeGuestToPB(g *vmlib.GuestNetwork) *pb.GuestNetwork {
	if g == nil {
		return nil
	}
	return &pb.GuestNetwork{Address: g.Address, Gateway: g.Gateway, Dns: g.DNS, Configuration: g.Configuration}
}

func facadeGuestFromPB(g *pb.GuestNetwork) *vmlib.GuestNetwork {
	if g == nil {
		return nil
	}
	return &vmlib.GuestNetwork{Address: g.GetAddress(), Gateway: g.GetGateway(), DNS: g.GetDns(), Configuration: g.GetConfiguration()}
}

// ---- server -----------------------------------------------------------------

type facadeServer struct {
	pb.UnimplementedVMServiceServer
	l       vmlib.Lifecycle
	opLimit time.Duration
}

// FacadeOption configures RegisterFacade.
type FacadeOption func(*facadeServer)

// WithOperationTimeout bounds how long the server waits inside Stop, Commit and
// Delete (for example for an exit receipt). It lets the server answer with a
// typed, structured "retained" result before a longer client deadline expires.
// It bounds the call only; it never stops a VM.
func WithOperationTimeout(d time.Duration) FacadeOption {
	return func(s *facadeServer) { s.opLimit = d }
}

func (s *facadeServer) bound(ctx context.Context) (context.Context, context.CancelFunc) {
	if s.opLimit <= 0 {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, s.opLimit)
}

// RegisterFacade exposes a lifecycle facade (any vmlib.Lifecycle, normally a
// *vmlib.Service over a local or remote backend).
func RegisterFacade(s grpc.ServiceRegistrar, l vmlib.Lifecycle, opts ...FacadeOption) {
	srv := &facadeServer{l: l, opLimit: 5 * time.Minute}
	for _, o := range opts {
		o(srv)
	}
	pb.RegisterVMServiceServer(s, srv)
}

func (s *facadeServer) CreateVM(ctx context.Context, r *pb.CreateVMRequest) (*pb.VM, error) {
	if err := vmlib.ValidateID("vm", r.GetVmId()); err != nil {
		return nil, toStatus(err) // the client allocates the ID so a lost response is recoverable
	}
	if len(r.GetSshKeyReference()) > 128 {
		return nil, toStatus(vmlib.ErrInvalidArgument)
	}
	for _, c := range [][2]string{{"image", r.GetImageId()}, {"profile", r.GetProfile()}} {
		if err := vmlib.ValidateID(c[0], c[1]); err != nil {
			return nil, toStatus(err)
		}
	}
	if err := vmlib.ValidatePublicKey(r.GetSshPublicKey()); err != nil {
		return nil, toStatus(err)
	}
	if len(r.GetLabels()) > 32 {
		return nil, toStatus(vmlib.ErrInvalidArgument)
	}
	// The client's RPC context bounds this call only; Service never ties a
	// running VM's lifetime to it.
	vm, err := s.l.Create(ctx, vmlib.CreateRequest{ID: r.GetVmId(), ImageID: r.GetImageId(), Profile: r.GetProfile(),
		Labels: r.GetLabels(), SSHPublicKey: r.GetSshPublicKey(), SSHKeyReference: r.GetSshKeyReference()})
	if err != nil {
		return nil, toStatusWithVM(err, vm) // ambiguous failures keep the VM id
	}
	return vmToPB(vm), nil
}

func (s *facadeServer) vm(ctx context.Context, id string, f func(context.Context, string) (vmlib.VM, error)) (*pb.VM, error) {
	if err := vmlib.ValidateID("vm", id); err != nil {
		return nil, toStatus(err)
	}
	v, err := f(ctx, id)
	if err != nil {
		return nil, toStatus(err)
	}
	return vmToPB(v), nil
}

func (s *facadeServer) GetVM(ctx context.Context, r *pb.VMRef) (*pb.VM, error) {
	return s.vm(ctx, r.GetVmId(), s.l.Get)
}
func (s *facadeServer) StopVM(ctx context.Context, r *pb.VMRef) (*pb.VM, error) {
	ctx, cancel := s.bound(ctx)
	defer cancel()
	return s.vm(ctx, r.GetVmId(), s.l.Stop)
}

func (s *facadeServer) ListVMs(ctx context.Context, _ *pb.ListVMsRequest) (*pb.ListVMsResponse, error) {
	vms, err := s.l.List(ctx)
	if err != nil {
		return nil, toStatus(err)
	}
	out := &pb.ListVMsResponse{}
	for _, v := range vms {
		out.Vms = append(out.Vms, vmToPB(v))
	}
	return out, nil
}

func (s *facadeServer) GetSSHInfo(ctx context.Context, r *pb.VMRef) (*pb.SSHInfo, error) {
	if err := vmlib.ValidateID("vm", r.GetVmId()); err != nil {
		return nil, toStatus(err)
	}
	i, err := s.l.SSHInfo(ctx, r.GetVmId())
	if err != nil {
		return nil, toStatus(err)
	}
	return sshToPB(i), nil
}

func (s *facadeServer) CommitVM(ctx context.Context, r *pb.CommitVMRequest) (*pb.Image, error) {
	if err := vmlib.ValidateID("vm", r.GetVmId()); err != nil {
		return nil, toStatus(err)
	}
	if err := vmlib.ValidateID("image", r.GetNewImageId()); err != nil {
		return nil, toStatus(err)
	}
	ctx, cancel := s.bound(ctx)
	defer cancel()
	i, err := s.l.Commit(ctx, r.GetVmId(), r.GetNewImageId())
	if err != nil {
		return nil, toStatus(err)
	}
	return facadeImageToPB(i), nil
}

func (s *facadeServer) DeleteVM(ctx context.Context, r *pb.VMRef) (*pb.DeleteVMResponse, error) {
	if err := vmlib.ValidateID("vm", r.GetVmId()); err != nil {
		return nil, toStatus(err)
	}
	ctx, cancel := s.bound(ctx)
	defer cancel()
	res, err := s.l.Delete(ctx, r.GetVmId())
	if err != nil {
		// A retained disk keeps its structured result in the error metadata.
		return nil, withDeleteMetadata(toStatus(err), res)
	}
	return &pb.DeleteVMResponse{Deleted: res.Deleted, Retained: res.Retained, Detail: res.Detail}, nil
}

func (s *facadeServer) ListImages(ctx context.Context, _ *pb.ListImagesRequest) (*pb.ListImagesResponse, error) {
	imgs, err := s.l.ListImages(ctx)
	if err != nil {
		return nil, toStatus(err)
	}
	out := &pb.ListImagesResponse{}
	for _, i := range imgs {
		out.Images = append(out.Images, facadeImageToPB(i))
	}
	return out, nil
}

func withDeleteMetadata(base error, res vmlib.DeleteResult) error {
	st, _ := status.FromError(base)
	reason := ""
	for _, d := range st.Details() {
		if info, ok := d.(*errdetails.ErrorInfo); ok {
			reason = info.Reason
		}
	}
	return withMetadata(status.New(st.Code(), st.Message()), reason, map[string]string{
		"deleted": fmt.Sprint(res.Deleted), "retained": fmt.Sprint(res.Retained), "detail": res.Detail})
}

// ---- client -----------------------------------------------------------------

// FacadeClient implements vmlib.Lifecycle over generated gRPC+mTLS. It needs no
// KVM, Firecracker SDK or host access.
type FacadeClient struct {
	conn   *grpc.ClientConn
	c      pb.VMServiceClient
	prober vmlib.SSHProber
	poll   time.Duration
}

var _ vmlib.Lifecycle = (*FacadeClient)(nil)

// ClientOption configures a FacadeClient.
type ClientOption func(*FacadeClient)

// WithSSHProber sets the CLIENT-side readiness probe used by WaitSSHReady. A
// remote client usually cannot dial the host-private guest, so pass something
// that goes through the bastion, e.g. vmlib.SSHCommandProber{...}.Probe.
func WithSSHProber(p vmlib.SSHProber) ClientOption { return func(c *FacadeClient) { c.prober = p } }

// WithPollInterval sets the WaitSSHReady polling period.
func WithPollInterval(d time.Duration) ClientOption { return func(c *FacadeClient) { c.poll = d } }

// WaitSSHReady polls with the client-side prober (default: TCP dial, which only
// helps where the guest address is routable from this process).
func (c *FacadeClient) WaitSSHReady(ctx context.Context, id string) (vmlib.SSHInfo, error) {
	return vmlib.WaitReady(ctx, c, id, c.prober, c.poll)
}

func DialFacade(addr string, cfg *tls.Config, clientOpts ...ClientOption) (*FacadeClient, error) {
	opts, err := dialOptions(cfg)
	if err != nil {
		return nil, err
	}
	conn, err := grpc.Dial(addr, opts...)
	if err != nil {
		return nil, err
	}
	c := &FacadeClient{conn: conn, c: pb.NewVMServiceClient(conn), prober: vmlib.TCPProber, poll: time.Second}
	for _, o := range clientOpts {
		o(c)
	}
	return c, nil
}

func (c *FacadeClient) Close() error { return c.conn.Close() }

func (c *FacadeClient) vm(r *pb.VM, err error) (vmlib.VM, error) {
	if err != nil {
		return vmlib.VM{}, fromStatus(err)
	}
	return vmFromPB(r), nil
}

func (c *FacadeClient) Create(ctx context.Context, req vmlib.CreateRequest) (vmlib.VM, error) {
	// The client allocates the ID so that a lost or ambiguous response can always
	// be reconciled with Get/Stop/Delete. SSHKeyReference is a non-secret label.
	if req.ID == "" {
		id, err := newVMID()
		if err != nil {
			return vmlib.VM{}, err
		}
		req.ID = id
	}
	r, err := c.c.CreateVM(ctx, &pb.CreateVMRequest{VmId: req.ID, ImageId: req.ImageID, Profile: req.Profile,
		Labels: req.Labels, SshPublicKey: req.SSHPublicKey, SshKeyReference: req.SSHKeyReference})
	if err != nil {
		partial := vmlib.VM{ID: req.ID, ImageID: req.ImageID, Profile: req.Profile}
		if md := errMetadata(err); md != nil {
			partial.State, partial.Retained, partial.RetainedReason = vmlib.VMState(md["vm_state"]), md["retained"] == "true", md["retained_reason"]
		}
		return partial, fromStatus(err)
	}
	return vmFromPB(r), nil
}

func (c *FacadeClient) Get(ctx context.Context, id string) (vmlib.VM, error) {
	return c.vm(c.c.GetVM(ctx, &pb.VMRef{VmId: id}))
}
func (c *FacadeClient) Stop(ctx context.Context, id string) (vmlib.VM, error) {
	return c.vm(c.c.StopVM(ctx, &pb.VMRef{VmId: id}))
}

func (c *FacadeClient) List(ctx context.Context) ([]vmlib.VM, error) {
	r, err := c.c.ListVMs(ctx, &pb.ListVMsRequest{})
	if err != nil {
		return nil, fromStatus(err)
	}
	var out []vmlib.VM
	for _, v := range r.GetVms() {
		out = append(out, vmFromPB(v))
	}
	return out, nil
}

func (c *FacadeClient) SSHInfo(ctx context.Context, id string) (vmlib.SSHInfo, error) {
	r, err := c.c.GetSSHInfo(ctx, &pb.VMRef{VmId: id})
	if err != nil {
		return vmlib.SSHInfo{}, fromStatus(err)
	}
	return sshFromPB(r), nil
}

func (c *FacadeClient) Commit(ctx context.Context, id, newID string) (vmlib.Image, error) {
	r, err := c.c.CommitVM(ctx, &pb.CommitVMRequest{VmId: id, NewImageId: newID})
	if err != nil {
		return vmlib.Image{}, fromStatus(err)
	}
	return facadeImageFromPB(r), nil
}

func (c *FacadeClient) Delete(ctx context.Context, id string) (vmlib.DeleteResult, error) {
	r, err := c.c.DeleteVM(ctx, &pb.VMRef{VmId: id})
	if err != nil {
		var res vmlib.DeleteResult
		if md := errMetadata(err); md != nil {
			res = vmlib.DeleteResult{Deleted: md["deleted"] == "true", Retained: md["retained"] == "true", Detail: md["detail"]}
		}
		return res, fromStatus(err)
	}
	return vmlib.DeleteResult{Deleted: r.GetDeleted(), Retained: r.GetRetained(), Detail: r.GetDetail()}, nil
}

func (c *FacadeClient) ListImages(ctx context.Context) ([]vmlib.Image, error) {
	r, err := c.c.ListImages(ctx, &pb.ListImagesRequest{})
	if err != nil {
		return nil, fromStatus(err)
	}
	var out []vmlib.Image
	for _, i := range r.GetImages() {
		out = append(out, facadeImageFromPB(i))
	}
	return out, nil
}

func newVMID() (string, error) {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "vm-" + hex.EncodeToString(b[:]), nil
}
